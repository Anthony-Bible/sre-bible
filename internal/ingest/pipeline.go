package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sync/errgroup"
)

// Embedder produces vector embeddings for a batch of texts.
// Defined here per the "accept interfaces" guideline — consumed by Pipeline.
type Embedder interface {
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
}

// PDFExtractor extracts plain text from a PDF file path.
type PDFExtractor interface {
	ExtractPDFText(ctx context.Context, pdfPath string) (string, error)
}

// URLExtractor fetches a web page and returns its main text content.
type URLExtractor interface {
	ExtractURL(ctx context.Context, location string) (string, error)
}

// Describer generates a short natural-language summary of a source's content,
// used to help the agent choose documents in list_documents.
type Describer interface {
	Describe(ctx context.Context, text string) (string, error)
}

// Pipeline orchestrates extract → chunk → embed → store for a single source.
type Pipeline struct {
	pdfExtractor PDFExtractor
	urlExtractor URLExtractor
	embedder     Embedder
	describer    Describer
	store        SourceRepository
	log          *slog.Logger
}

// NewPipeline creates a Pipeline wired with the provided dependencies.
func NewPipeline(pdfExtractor PDFExtractor, embedder Embedder, describer Describer, urlExtractor URLExtractor, store SourceRepository, log *slog.Logger) *Pipeline {
	return &Pipeline{
		pdfExtractor: pdfExtractor,
		urlExtractor: urlExtractor,
		embedder:     embedder,
		describer:    describer,
		store:        store,
		log:          log,
	}
}

// Run ingests a single source (PDF path or URL) end-to-end.
func (p *Pipeline) Run(ctx context.Context, location string) error {
	name, srcType, err := DeriveSourceName(location)
	if err != nil {
		return fmt.Errorf("derive source name: %w", err)
	}

	p.log.InfoContext(ctx, "ingesting source", "name", name, "type", srcType)

	text, err := p.extractText(ctx, srcType, location)
	if err != nil {
		return fmt.Errorf("extract text from %s: %w", name, err)
	}

	// Chunk synchronously (pure CPU); describe and embed concurrently (both are network calls).
	segments := ChunkText(text)
	p.log.InfoContext(ctx, "chunked source", "name", name, "chunks", len(segments))

	var description string
	var embeddings [][]float32
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		description, err = p.describer.Describe(gctx, text)
		if err != nil {
			return fmt.Errorf("describe source %s: %w", name, err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		embeddings, err = p.embedder.EmbedDocuments(gctx, segments)
		if err != nil {
			return fmt.Errorf("embed chunks for %s: %w", name, err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}

	if len(embeddings) != len(segments) {
		return fmt.Errorf("embed %s: got %d embeddings for %d chunks", name, len(embeddings), len(segments))
	}

	chunks := make([]Chunk, len(segments))
	for i, seg := range segments {
		chunks[i] = Chunk{
			Idx:       i,
			Content:   seg,
			Embedding: embeddings[i],
		}
	}

	src := Source{Name: name, Type: srcType, Location: location, FullText: text, Description: description}
	if err := p.store.ReplaceSource(ctx, src, chunks); err != nil {
		return fmt.Errorf("store source %s: %w", name, err)
	}

	p.log.InfoContext(ctx, "source ingested", "name", name, "chunks", len(chunks))
	return nil
}

func (p *Pipeline) extractText(ctx context.Context, srcType, location string) (string, error) {
	switch srcType {
	case "pdf":
		return p.pdfExtractor.ExtractPDFText(ctx, location)
	case "url":
		return p.urlExtractor.ExtractURL(ctx, location)
	case "text":
		data, err := os.ReadFile(location)
		if err != nil {
			return "", fmt.Errorf("read text file: %w", err)
		}
		return string(data), nil
	default:
		return "", fmt.Errorf("unknown source type: %s", srcType)
	}
}

package ingest

import (
	"context"
	"fmt"
	"log/slog"
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

// Pipeline orchestrates extract → chunk → embed → store for a single source.
type Pipeline struct {
	pdfExtractor PDFExtractor
	urlExtractor URLExtractor
	embedder     Embedder
	store        SourceRepository
	log          *slog.Logger
}

// NewPipeline creates a Pipeline wired with the provided dependencies.
func NewPipeline(pdfExtractor PDFExtractor, embedder Embedder, urlExtractor URLExtractor, store SourceRepository, log *slog.Logger) *Pipeline {
	return &Pipeline{
		pdfExtractor: pdfExtractor,
		urlExtractor: urlExtractor,
		embedder:     embedder,
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

	segments := ChunkText(text)
	p.log.InfoContext(ctx, "chunked source", "name", name, "chunks", len(segments))

	embeddings, err := p.embedder.EmbedDocuments(ctx, segments)
	if err != nil {
		return fmt.Errorf("embed chunks for %s: %w", name, err)
	}

	chunks := make([]Chunk, len(segments))
	for i, seg := range segments {
		chunks[i] = Chunk{
			Idx:       i,
			Content:   seg,
			Embedding: embeddings[i],
		}
	}

	src := Source{Name: name, Type: srcType, Location: location, FullText: text}
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
	default:
		return "", fmt.Errorf("unknown source type: %s", srcType)
	}
}

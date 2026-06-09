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

// PIIScreener redacts personal information from extracted source text before it
// is chunked, embedded, or stored. Phone numbers, home addresses, email addresses,
// government IDs, and dates of birth are replaced with [redacted] in place.
// LinkedIn and GitHub URLs are preserved.
type PIIScreener interface {
	ScreenPII(ctx context.Context, text string) (string, error)
}

// Pipeline orchestrates extract → chunk → embed → store for a single source.
type Pipeline struct {
	pdfExtractor PDFExtractor
	urlExtractor URLExtractor
	embedder     Embedder
	describer    Describer
	screener     PIIScreener
	store        SourceRepository
	log          *slog.Logger
}

// NewPipeline creates a Pipeline wired with the provided dependencies.
func NewPipeline(pdfExtractor PDFExtractor, embedder Embedder, describer Describer, screener PIIScreener, urlExtractor URLExtractor, store SourceRepository, log *slog.Logger) *Pipeline {
	return &Pipeline{
		pdfExtractor: pdfExtractor,
		urlExtractor: urlExtractor,
		embedder:     embedder,
		describer:    describer,
		screener:     screener,
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

	// Screen for PII before any downstream use of the text. Redacted text flows
	// into chunk content, embeddings, the source description, and full_text —
	// all three sinks — because we mutate text before ChunkText and the errgroup.
	text, err = p.screener.ScreenPII(ctx, text)
	if err != nil {
		return fmt.Errorf("screen pii for %s: %w", name, err)
	}
	p.log.InfoContext(ctx, "pii screen complete", "name", name)

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

	src := Source{Name: name, Type: srcType, Location: location, FullText: text, Description: description}
	n, err := p.buildAndStore(ctx, src, segments, embeddings)
	if err != nil {
		return err
	}

	p.log.InfoContext(ctx, "source ingested", "name", name, "chunks", n)
	return nil
}

// buildAndStore pairs each segment with its embedding into a Chunk and atomically
// replaces src's stored chunks via ReplaceSource. It is the shared tail of Run
// (full ingest) and Rechunk (repair); the embeddings slice must align with
// segments by index. Returns the number of chunks written.
func (p *Pipeline) buildAndStore(ctx context.Context, src Source, segments []string, embeddings [][]float32) (int, error) {
	if len(embeddings) != len(segments) {
		return 0, fmt.Errorf("embed %s: got %d embeddings for %d chunks", src.Name, len(embeddings), len(segments))
	}

	chunks := make([]Chunk, len(segments))
	for i, seg := range segments {
		chunks[i] = Chunk{Idx: i, Content: seg, Embedding: embeddings[i]}
	}

	if err := p.store.ReplaceSource(ctx, src, chunks); err != nil {
		return 0, fmt.Errorf("store source %s: %w", src.Name, err)
	}
	return len(chunks), nil
}

// Rechunk re-segments an existing source from its already-extracted FullText and
// atomically replaces its stored chunks — without re-extracting, re-screening for
// PII, or re-describing. It exists to repair sources chunked by an older, buggy
// splitter: src.Name, Type, Location, and Description are persisted verbatim, so
// only the chunk rows (content + embeddings) change. FullText must be present
// (it is the sole input); a source with no stored full text cannot be rechunked.
// Returns the number of chunks written.
func (p *Pipeline) Rechunk(ctx context.Context, src Source) (int, error) {
	if src.FullText == "" {
		return 0, fmt.Errorf("rechunk %s: no stored full text", src.Name)
	}

	segments := ChunkText(src.FullText)
	if len(segments) == 0 {
		return 0, fmt.Errorf("rechunk %s: chunking produced no segments", src.Name)
	}

	embeddings, err := p.embedder.EmbedDocuments(ctx, segments)
	if err != nil {
		return 0, fmt.Errorf("embed chunks for %s: %w", src.Name, err)
	}

	n, err := p.buildAndStore(ctx, src, segments, embeddings)
	if err != nil {
		return 0, err
	}

	p.log.InfoContext(ctx, "source rechunked", "name", src.Name, "chunks", n)
	return n, nil
}

func (p *Pipeline) extractText(ctx context.Context, srcType, location string) (string, error) {
	switch srcType {
	case sourceTypePDF:
		return p.pdfExtractor.ExtractPDFText(ctx, location)
	case sourceTypeURL:
		return p.urlExtractor.ExtractURL(ctx, location)
	case sourceTypeText:
		data, err := os.ReadFile(location)
		if err != nil {
			return "", fmt.Errorf("read text file: %w", err)
		}
		return string(data), nil
	default:
		return "", fmt.Errorf("unknown source type: %s", srcType)
	}
}

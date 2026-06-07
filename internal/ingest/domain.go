package ingest

import "context"

// Source is a document or URL in the knowledge base.
// ID is absent — it is a database implementation detail, invisible to the domain.
type Source struct {
	Name        string
	Type        string
	Location    string
	FullText    string // complete extracted text; empty for legacy rows
	Description string // LLM-generated 1–2 sentence summary; empty for legacy rows
}

// Chunk is a contiguous text segment with its vector embedding.
// Callers assign sequential Idx values starting at 0.
type Chunk struct {
	Idx       int
	Content   string
	Embedding []float32
}

// SourceRepository is the storage port consumed by Pipeline.
// Interface lives in the consumer package per the "accept interfaces" guideline.
type SourceRepository interface {
	ReplaceSource(ctx context.Context, src Source, chunks []Chunk) error
}

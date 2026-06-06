package rag

import (
	"context"
	"log/slog"
)

const defaultK = 8

// Pipeline wires together embedding, retrieval, and generation.
type Pipeline struct {
	embedder  QueryEmbedder
	searcher  ChunkSearcher
	generator Generator
	k         int
	log       *slog.Logger
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, k int, log *slog.Logger) *Pipeline {
	if k <= 0 {
		k = defaultK
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{
		embedder:  embedder,
		searcher:  searcher,
		generator: generator,
		k:         k,
		log:       log,
	}
}

// Answer embeds the question, retrieves relevant chunks, assembles the full
// message history, streams a grounded response via onToken, and returns
// deduplicated citation source names.
//
// history contains prior turns from the Session (may be empty for first turn).
// citations are returned after streaming completes; they are derived from
// retrieved chunks, not from Claude's output.
func (p *Pipeline) Answer(ctx context.Context, history []Message, question string, onToken func(string) error) ([]string, error) {
	queryVec, err := p.embedder.EmbedQuery(ctx, question)
	if err != nil {
		return nil, err
	}

	chunks, err := p.searcher.SearchChunks(ctx, queryVec, p.k)
	if err != nil {
		return nil, err
	}

	if len(chunks) == 0 {
		if err := onToken("I couldn't find relevant information in my knowledge base to answer that question."); err != nil {
			return nil, err
		}
		return nil, nil
	}

	currentMsg := BuildUserMessage(question, chunks)

	messages := make([]Message, len(history)+1)
	copy(messages, history)
	messages[len(history)] = currentMsg

	if err := p.generator.StreamAnswer(ctx, messages, onToken); err != nil {
		return nil, err
	}

	var citations []string
	seen := make(map[string]struct{})
	for _, c := range chunks {
		if _, ok := seen[c.SourceName]; !ok {
			seen[c.SourceName] = struct{}{}
			citations = append(citations, c.SourceName)
		}
	}

	p.log.InfoContext(ctx, "query answered", "chunks", len(chunks), "citations", len(citations))
	return citations, nil
}

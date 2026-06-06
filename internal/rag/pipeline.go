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
	lister    DocumentLister
	fetcher   FullTextFetcher
	k         int
	log       *slog.Logger
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
// lister and fetcher may be nil; when both are non-nil the model may invoke
// the list_documents / fetch_full_document tools to escalate beyond chunks.
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, lister DocumentLister, fetcher FullTextFetcher, k int, log *slog.Logger) *Pipeline {
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
		lister:    lister,
		fetcher:   fetcher,
		k:         k,
		log:       log,
	}
}

// Answer embeds the question, retrieves relevant chunks, assembles the full
// message history, streams a grounded response via onToken, and returns
// deduplicated citation source names.
//
// history contains prior turns from the Session (may be empty for first turn).
// onStatus, if non-nil, receives transient status messages during tool rounds.
// citations are returned after streaming completes; they are derived from
// retrieved chunks, not from the model's tool fetches (v1 known limitation).
func (p *Pipeline) Answer(ctx context.Context, history []Message, question string, onToken func(string) error, onStatus func(string) error) ([]string, error) {
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

	tools := ToolSet{Lister: p.lister, Fetcher: p.fetcher}
	if err := p.generator.StreamAnswer(ctx, messages, tools, onToken, onStatus); err != nil {
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

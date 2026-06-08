package rag

import (
	"context"
	"log/slog"
)

const defaultK = 8

// Pipeline wires together embedding, retrieval, and generation.
type Pipeline struct {
	embedder   QueryEmbedder
	searcher   ChunkSearcher
	generator  Generator
	lister     DocumentLister
	fetcher    FullTextFetcher
	emailerFor EmailerFactory
	k          int
	log        *slog.Logger
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
// lister and fetcher may be nil; when both are non-nil the model may invoke
// the list_documents / fetch_full_document tools to escalate beyond chunks.
// emailerFor may be nil; when non-nil, the send_contact_email tool is advertised.
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, lister DocumentLister, fetcher FullTextFetcher, emailerFor EmailerFactory, k int, log *slog.Logger) *Pipeline {
	if k <= 0 {
		k = defaultK
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{
		embedder:   embedder,
		searcher:   searcher,
		generator:  generator,
		lister:     lister,
		fetcher:    fetcher,
		emailerFor: emailerFor,
		k:          k,
		log:        log,
	}
}

// Answer embeds the question, retrieves relevant chunks, assembles the full
// message history, streams a grounded response via onToken, and returns
// deduplicated citation source names.
//
// sessionID identifies the current session; used to create a session-bound
// EmailSender when an emailerFor factory is configured.
// history contains prior turns from the Session (may be empty for first turn).
// onTrace, if non-nil, receives each TraceStep in order: the retrieval step (always,
// including the zero-chunk path), then the generator's tool_call and answer steps.
// citations include both vector-retrieved chunk sources and any documents fetched
// via the fetch_full_document tool during generation.
func (p *Pipeline) Answer(ctx context.Context, sessionID string, history []Message, question string, onToken func(string) error, onTrace func(TraceStep) error) ([]string, error) {
	queryVec, err := p.embedder.EmbedQuery(ctx, question)
	if err != nil {
		return nil, err
	}

	chunks, err := p.searcher.SearchChunks(ctx, queryVec, p.k)
	if err != nil {
		return nil, err
	}

	// Emit the retrieval step on BOTH paths — before the zero-chunk branch — so it
	// fires even on early return. This is the only place chunk Content is available
	// before BuildUserMessage consumes it, so the grounding excerpts are captured here.
	if onTrace != nil {
		if err := onTrace(buildRetrievalStep(chunks)); err != nil {
			return nil, err
		}
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
	if p.emailerFor != nil {
		tools.Emailer = p.emailerFor(sessionID)
	}
	toolFetched, err := p.generator.StreamAnswer(ctx, messages, tools, onToken, onTrace)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var citations []string
	for _, c := range chunks {
		if _, ok := seen[c.SourceName]; !ok {
			seen[c.SourceName] = struct{}{}
			citations = append(citations, c.SourceName)
		}
	}
	for _, name := range toolFetched {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			citations = append(citations, name)
		}
	}

	p.log.InfoContext(ctx, "query answered", "chunks", len(chunks), "citations", len(citations))
	return citations, nil
}

// buildRetrievalStep turns the retrieved chunks into a TraceStep of kind retrieval:
// one GroundingExcerpt per chunk (source + raw content), the chunk count, and the
// count of distinct sources. Excerpts is always non-nil (empty on the zero-chunk path)
// so it serialises to a JSON array rather than null.
func buildRetrievalStep(chunks []RetrievedChunk) TraceStep {
	excerpts := make([]GroundingExcerpt, 0, len(chunks))
	seen := make(map[string]struct{}, len(chunks))
	for _, c := range chunks {
		excerpts = append(excerpts, GroundingExcerpt{SourceName: c.SourceName, Text: c.Content})
		seen[c.SourceName] = struct{}{}
	}
	return TraceStep{
		Kind:  TraceKindRetrieval,
		Label: "Searched knowledge base",
		Retrieval: &RetrievalDetail{
			ChunkCount:  len(chunks),
			SourceCount: len(seen),
			Excerpts:    excerpts,
		},
	}
}

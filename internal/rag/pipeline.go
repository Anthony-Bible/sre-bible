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
	matcher    JobMatcher
	emailerFor EmailerFactory
	k          int
	log        *slog.Logger
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
// lister and fetcher may be nil; when both are non-nil the model may invoke
// the list_documents / fetch_full_document tools to escalate beyond chunks.
// matcher may be nil; when non-nil, the match_job_description tool is advertised.
// emailerFor may be nil; when non-nil, the send_contact_email tool is advertised.
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, lister DocumentLister, fetcher FullTextFetcher, matcher JobMatcher, emailerFor EmailerFactory, k int, log *slog.Logger) *Pipeline {
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
		matcher:    matcher,
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
	// Trace emission is best-effort: a failed transient write must not abort the answer
	// (cancellation is handled via ctx, token streaming via onToken).
	if onTrace != nil {
		_ = onTrace(buildRetrievalStep(chunks))
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

	tools := ToolSet{Lister: p.lister, Fetcher: p.fetcher, Matcher: p.matcher}
	if p.emailerFor != nil {
		tools.Emailer = p.emailerFor(sessionID)
	}
	toolFetched, err := p.generator.StreamAnswer(ctx, messages, tools, onToken, onTrace)
	if err != nil {
		return nil, err
	}

	// Citations = vector-retrieved chunk sources first (in retrieval order), then any
	// documents the model fetched via tools, all deduped by DedupeSourceNames.
	names := make([]string, 0, len(chunks)+len(toolFetched))
	for _, c := range chunks {
		names = append(names, c.SourceName)
	}
	names = append(names, toolFetched...)
	citations := DedupeSourceNames(names)

	p.log.InfoContext(ctx, "query answered", "chunks", len(chunks), "citations", len(citations))
	return citations, nil
}

// buildRetrievalStep turns the retrieved chunks into a TraceStep of kind retrieval:
// one GroundingExcerpt per chunk (source + raw content), the chunk count, and the
// count of distinct sources. Excerpts is always non-nil (empty on the zero-chunk path)
// so it serialises to a JSON array rather than null.
func buildRetrievalStep(chunks []RetrievedChunk) TraceStep {
	excerpts := make([]GroundingExcerpt, 0, len(chunks))
	names := make([]string, 0, len(chunks))
	for _, c := range chunks {
		excerpts = append(excerpts, GroundingExcerpt{SourceName: c.SourceName, Text: c.Content})
		names = append(names, c.SourceName)
	}
	return TraceStep{
		Kind:  TraceKindRetrieval,
		Label: "Searched knowledge base",
		Retrieval: &RetrievalDetail{
			ChunkCount:  len(chunks),
			SourceCount: len(DedupeSourceNames(names)),
			Excerpts:    excerpts,
		},
	}
}

// DedupeSourceNames returns the distinct source names from names, preserving
// first-seen order. It is the single citation-attribution primitive shared across
// retrieval paths: Pipeline.Answer (chunk sources then tool-fetched docs), the
// retrieval TraceStep's distinct-source count, and the match_job_description tool
// (evidence sources across requirements) all build their source lists through it,
// so every path attributes identically. Returns a non-nil empty slice for empty input.
func DedupeSourceNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

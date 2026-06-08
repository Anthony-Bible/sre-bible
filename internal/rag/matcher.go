package rag

import "context"

// defaultMatchK is the per-Requirement retrieval depth when the caller passes k<=0.
// Smaller than the conversational defaultK (8): a Fit Scorecard wants a few tightly
// relevant excerpts per Requirement, not a broad sweep.
const defaultMatchK = 4

// Matcher adapts a QueryEmbedder + ChunkSearcher into a JobMatcher: it embeds a
// single Requirement with RETRIEVAL_QUERY semantics and returns the most similar
// Chunks. It performs no LLM call — Requirement decomposition and Match-class
// synthesis are the generating model's job, not this adapter's.
type Matcher struct {
	embedder QueryEmbedder
	searcher ChunkSearcher
}

// NewMatcher wires an embedder and searcher into a JobMatcher.
func NewMatcher(embedder QueryEmbedder, searcher ChunkSearcher) *Matcher {
	return &Matcher{embedder: embedder, searcher: searcher}
}

// MatchRequirement embeds the requirement and returns the k most similar Chunks.
// k<=0 defaults to defaultMatchK. An empty (non-nil error) result means no
// evidence was found — the caller treats that as a Gap.
func (m *Matcher) MatchRequirement(ctx context.Context, requirement string, k int) ([]RetrievedChunk, error) {
	if k <= 0 {
		k = defaultMatchK
	}
	vec, err := m.embedder.EmbedQuery(ctx, requirement)
	if err != nil {
		return nil, err
	}
	return m.searcher.SearchChunks(ctx, vec, k)
}

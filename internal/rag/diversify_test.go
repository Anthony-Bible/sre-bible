package rag

import "testing"

// chunksFrom builds a similarity-ordered pool from a list of source names; the content
// encodes the source so callers can assert exact identity and ordering.
func chunksFrom(sources ...string) []RetrievedChunk {
	out := make([]RetrievedChunk, len(sources))
	for i, s := range sources {
		out[i] = RetrievedChunk{SourceName: s, Content: s}
	}
	return out
}

func sourceNames(chunks []RetrievedChunk) []string {
	names := make([]string, len(chunks))
	for i, c := range chunks {
		names[i] = c.SourceName
	}
	return names
}

func sourceCounts(chunks []RetrievedChunk) map[string]int {
	counts := map[string]int{}
	for _, c := range chunks {
		counts[c.SourceName]++
	}
	return counts
}

// isSubsequence reports whether sub appears in seq in the same relative order — the
// contract that diversifyChunks preserves the pool's similarity ordering in its output.
func isSubsequence(sub, seq []string) bool {
	j := 0
	for _, s := range seq {
		if j < len(sub) && sub[j] == s {
			j++
		}
	}
	return j == len(sub)
}

func TestDiversifyChunks(t *testing.T) {
	tests := []struct {
		name         string
		pool         []RetrievedChunk
		k            int
		maxPerSource int
		wantLen      int
		// wantPerSourceAtMost, when >0, asserts no source exceeds this count. Only set it
		// when the capped pass alone fills k (no backfill expected).
		wantPerSourceAtMost int
	}{
		{name: "empty pool returns nil", pool: nil, k: 8, wantLen: 0},
		{name: "non-positive k returns nil", pool: chunksFrom("a", "b"), k: 0, wantLen: 0},
		{name: "pool within k is returned whole", pool: chunksFrom("a", "a", "b"), k: 8, wantLen: 3},
		{
			name: "cap disabled truncates to k by similarity",
			pool: chunksFrom("a", "a", "a", "a", "a"), k: 3, maxPerSource: 0, wantLen: 3,
		},
		{
			name: "cap enforced when enough distinct sources exist",
			pool: chunksFrom("a", "a", "a", "a", "b", "b", "c", "d", "e", "f"),
			k:    8, maxPerSource: 3, wantLen: 8, wantPerSourceAtMost: 3,
		},
		{
			name: "backfill from over-cap chunks when too few sources to fill k",
			pool: chunksFrom("a", "a", "a", "a", "a", "a", "a", "a", "b", "b"),
			k:    8, maxPerSource: 3, wantLen: 8, // 3 'a' + 2 'b' + 3 'a' backfilled
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diversifyChunks(tt.pool, tt.k, tt.maxPerSource)
			assertDiversified(t, got, tt.pool, tt.wantLen, tt.wantPerSourceAtMost)
		})
	}
}

func assertDiversified(t *testing.T, got, pool []RetrievedChunk, wantLen, perSourceAtMost int) {
	t.Helper()
	if len(got) != wantLen {
		t.Fatalf("len = %d, want %d (%v)", len(got), wantLen, sourceNames(got))
	}
	if !isSubsequence(sourceNames(got), sourceNames(pool)) {
		t.Errorf("result %v is not an order-preserving subsequence of pool %v", sourceNames(got), sourceNames(pool))
	}
	if perSourceAtMost <= 0 {
		return
	}
	for src, n := range sourceCounts(got) {
		if n > perSourceAtMost {
			t.Errorf("source %q appears %d times, want at most %d", src, n, perSourceAtMost)
		}
	}
}

// TestDiversifyChunksBreaksMonopoly is the regression guard for the original complaint:
// one densely-ingested source must not monopolise the top-k when other relevant sources
// exist. The pool is front-loaded with "rca" (mirroring how the RCA-agent chunks, at 31%
// of the real corpus, dominate raw similarity order), but enough distinct non-rca sources
// follow to fill k under the cap. With cap=3 and k=8, rca is held to its cap and the
// grounding set spans many sources — exactly the rebalancing the change exists to produce.
func TestDiversifyChunksBreaksMonopoly(t *testing.T) {
	pool := chunksFrom(
		"rca", "rca", "rca", "rca", "rca",
		"resume", "blog", "brag", "postgres", "codechunking",
	)
	got := diversifyChunks(pool, 8, 3)
	counts := sourceCounts(got)

	if counts["rca"] > 3 {
		t.Errorf("rca contributed %d chunks, want at most 3 (the cap): %v", counts["rca"], sourceNames(got))
	}
	if len(counts) < 4 {
		t.Errorf("only %d distinct sources in grounding set, want >= 4: %v", len(counts), sourceNames(got))
	}
}

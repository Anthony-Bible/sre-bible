package ingest

import (
	"strings"
	"testing"
	"unicode"
)

// makeText returns a string of approximately n bytes built from repeated
// English-looking words so tests have realistic prose-like input.
func makeText(n int) string {
	word := "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua "
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(word)
	}
	return b.String()[:n]
}

// makeParagraphs builds a string of roughly paragraphCount paragraphs each
// paragraphSize bytes long, joined with double-newlines.
func makeParagraphs(paragraphCount, paragraphSize int) string {
	para := makeText(paragraphSize)
	parts := make([]string, paragraphCount)
	for i := range parts {
		parts[i] = para
	}
	return strings.Join(parts, "\n\n")
}

// -----------------------------------------------------------------------
// Contract 1: every chunk is ≤ chunkHardCap (1200) chars
// -----------------------------------------------------------------------

func TestChunk_HardCapEnforced(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"slightly over cap", makeText(1300)},
		{"exactly double cap", makeText(2400)},
		{"large input", makeText(10000)},
		{"large paragraphed input", makeParagraphs(20, 800)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.input)
			if len(chunks) == 0 {
				t.Fatal("Chunk returned nil/empty slice for non-empty input")
			}
			for i, c := range chunks {
				if len(c) > chunkHardCap {
					t.Errorf("chunk[%d] length %d exceeds hard cap %d", i, len(c), chunkHardCap)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------
// Contract 2: consecutive chunks share ~200 chars of overlap
// -----------------------------------------------------------------------

func TestChunk_OverlapBetweenConsecutiveChunks(t *testing.T) {
	// Use a large enough input to produce at least 3 chunks.
	input := makeText(5000)
	chunks := ChunkText(input)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d — cannot verify overlap", len(chunks))
	}

	for i := range len(chunks) - 1 {
		if !overlapInRange(chunks[i], chunks[i+1], chunkOverlap-50, chunkOverlap+50) {
			actual := measureOverlap(chunks[i], chunks[i+1])
			t.Errorf(
				"chunks[%d] and chunks[%d]: overlap is %d chars, want %d±50",
				i, i+1, actual, chunkOverlap,
			)
		}
	}
}

// overlapInRange reports whether any overlap length in [lo, hi] makes the tail
// of current equal the prefix of next.
func overlapInRange(current, next string, lo, hi int) bool {
	for ovl := lo; ovl <= hi; ovl++ {
		if ovl > len(current) || ovl > len(next) {
			continue
		}
		if strings.HasPrefix(next, current[len(current)-ovl:]) {
			return true
		}
	}
	return false
}

// measureOverlap returns the actual number of chars shared between the tail of
// current and the prefix of next.
func measureOverlap(current, next string) int {
	limit := min(len(current), len(next))
	for ovl := limit; ovl > 0; ovl-- {
		if strings.HasPrefix(next, current[len(current)-ovl:]) {
			return ovl
		}
	}
	return 0
}

// -----------------------------------------------------------------------
// Contract 3: no empty or whitespace-only chunks
// -----------------------------------------------------------------------

func TestChunk_NoEmptyChunks(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"normal prose", makeText(3000)},
		{"paragraphed input", makeParagraphs(5, 600)},
		{"just over cap", makeText(1201)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.input)
			if len(chunks) == 0 {
				t.Fatal("Chunk returned nil/empty slice for non-empty input")
			}
			for i, c := range chunks {
				if strings.TrimFunc(c, unicode.IsSpace) == "" {
					t.Errorf("chunk[%d] is empty or whitespace-only", i)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------
// Contract 4: full coverage — every non-whitespace rune in the input
// appears in at least one chunk
// -----------------------------------------------------------------------

func TestChunk_FullCoverage(t *testing.T) {
	// We verify coverage by reconstructing the non-whitespace content from
	// all chunks and checking that the input's non-whitespace content is a
	// substring sequence present across the union of chunks.
	//
	// Simpler and contract-correct approach: every non-whitespace rune in the
	// input must appear in at least one chunk. We do this by checking that
	// the concatenated chunks (stripped of duplicate overlap) contain all
	// non-whitespace runes in the input.
	//
	// Because overlap complicates reconstruction we use a positional check:
	// for each position in the input, if it is non-whitespace, we verify that
	// the rune at that position is found in at least one chunk.
	//
	// A robust way: scan the input and verify that removing duplicates from
	// the union of all chunk content gives back every non-whitespace rune.
	// The simplest and unambiguous contract: the set of non-whitespace
	// characters in the input equals the multiset union of all chunks (ignoring
	// order). We check via frequency counting.

	input := makeParagraphs(8, 400)
	chunks := ChunkText(input)
	if len(chunks) == 0 {
		t.Fatal("Chunk returned nil/empty slice for non-empty input")
	}

	// Count non-whitespace runes in input.
	inputFreq := make(map[rune]int)
	for _, r := range input {
		if !unicode.IsSpace(r) {
			inputFreq[r]++
		}
	}

	// Count non-whitespace runes across all chunks.
	chunkFreq := make(map[rune]int)
	for _, c := range chunks {
		for _, r := range c {
			if !unicode.IsSpace(r) {
				chunkFreq[r]++
			}
		}
	}

	// Every rune that appears in the input must appear in at least one chunk.
	for r, inputCount := range inputFreq {
		chunkCount := chunkFreq[r]
		if chunkCount < inputCount {
			t.Errorf(
				"rune %q appears %d times in input but only %d times across all chunks",
				r, inputCount, chunkCount,
			)
		}
	}
}

// -----------------------------------------------------------------------
// Contract 5: empty or whitespace-only input returns nil
// -----------------------------------------------------------------------

func TestChunk_EmptyAndWhitespaceInputReturnsNil(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"single space", " "},
		{"tabs and spaces", "\t  \t"},
		{"newlines only", "\n\n\n"},
		{"mixed whitespace", " \t\n\r\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := ChunkText(tc.input)
			if result != nil {
				t.Errorf("Chunk(%q) = %v, want nil", tc.input, result)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Contract 6: input ≤ 1200 chars is returned as a single chunk
// -----------------------------------------------------------------------

func TestChunk_ShortInputReturnsSingleChunk(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"exactly at hard cap", makeText(chunkHardCap)},
		{"under target", makeText(chunkTarget - 1)},
		{"at target", makeText(chunkTarget)},
		{"between target and cap", makeText(chunkTarget + 100)},
		{"single word", "hello"},
		{"single sentence", "The quick brown fox jumps over the lazy dog."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.input)
			if len(chunks) != 1 {
				t.Errorf("Chunk returned %d chunks for input of length %d, want exactly 1", len(chunks), len(tc.input))
			}
			if len(chunks) == 1 && chunks[0] != tc.input {
				t.Errorf("single chunk content does not match input verbatim")
			}
		})
	}
}

// -----------------------------------------------------------------------
// Contract 8: an early paragraph boundary followed by a long run with only
// single-newline (list) or no breaks must not produce a "staircase" of
// ever-shrinking degenerate tail chunks.
//
// Regression for the resume/README bug: the experience section was a markdown
// list whose only \n\n was the section header. The chunker split early at that
// header, then the overlap window kept re-selecting the same boundary, emitting
// a chunk that shrank one word/char at a time ("Present*" -> "resent*" -> ...
// -> "*"). Every successive split point must strictly advance.
// -----------------------------------------------------------------------

func TestChunk_NoStaircaseAfterEarlyParagraphBreak(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			// Short header paragraph, then a long single-\n markdown list with
			// no further \n\n for well over the hard cap — mirrors the resume.
			name: "header then long single-newline list",
			input: "## EXPERIENCE\n\n" +
				strings.TrimRight(strings.Repeat(
					"* Built and shipped agentic systems that cut mean-time-to-resolution across services\n", 60), "\n"),
		},
		{
			// Early paragraph break, then one giant paragraph (prose, spaces only)
			// far exceeding the hard cap — the heading-then-prose variant.
			name:  "short heading then giant paragraph",
			input: "Overview\n\n" + makeText(5000),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.input)
			if len(chunks) == 0 {
				t.Fatal("ChunkText returned nil/empty for non-empty input")
			}

			// No degenerate fragments. The algorithm guarantees every chunk
			// (including the trailing remainder) carries at least one overlap
			// window of content; word-boundary snapping costs a few chars, so we
			// assert a floor comfortably below a real chunk yet far above the
			// 3–32 char staircase fragments the bug produced.
			const minLen = 100
			for i, c := range chunks {
				if l := len([]rune(c)); l < minLen {
					t.Errorf("chunk[%d] is a degenerate %d-char fragment (%q); staircase not eliminated", i, l, c)
				}
			}

			// Chunk count must be proportional to input size, not exploded by a
			// staircase. With step ≈ target-overlap (~800), a generous bound is
			// len/400 + 3.
			maxChunks := len([]rune(tc.input))/400 + 3
			if len(chunks) > maxChunks {
				t.Errorf("got %d chunks for %d-rune input; want ≤ %d (staircase suspected)",
					len(chunks), len([]rune(tc.input)), maxChunks)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Contract 7: splitting prefers paragraph boundary (\n\n) over word boundary
// -----------------------------------------------------------------------

func TestChunk_ParagraphBoundaryPreferredOverWordBoundary(t *testing.T) {
	// Build an input where the first paragraph ends well before chunkTarget
	// and the second paragraph would push past it. The split must land at
	// the paragraph boundary, not in the middle of the second paragraph.

	// Use distinct sentinel text for each paragraph so substring checks are
	// unambiguous — repeated lorem ipsum produces identical substrings across
	// both paragraphs, making containment checks unreliable.
	firstPara := strings.Repeat("alpha ", 800/6)  // ~800 chars of "alpha alpha …"
	secondPara := strings.Repeat("bravo ", 600/6) // ~600 chars of "bravo bravo …"
	// Pad to exact target lengths so the total exceeds hardCap.
	firstPara = (firstPara + strings.Repeat("alpha ", 200))[:800]
	secondPara = (secondPara + strings.Repeat("bravo ", 200))[:600]
	input := firstPara + "\n\n" + secondPara

	chunks := ChunkText(input)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// The first chunk must end at the paragraph boundary.
	// It should end with the content of firstPara (possibly trimmed).
	// It must NOT contain any content from secondPara beyond the overlap.
	firstChunk := chunks[0]

	// The paragraph boundary is at index 800. The first chunk should end
	// at or near that boundary (within the overlap window of 200 chars).
	// Specifically: the first chunk should NOT extend past the paragraph
	// break when a clean paragraph boundary exists within the target range.
	if !strings.Contains(firstChunk, firstPara[:50]) {
		t.Error("first chunk does not appear to contain the beginning of the first paragraph")
	}

	// The first chunk must end with the tail of firstPara, not mid-word
	// in the middle of secondPara's content. Because paragraphs use distinct
	// sentinel words ("alpha" vs "bravo"), any "bravo" token in the first
	// chunk means the split crossed the paragraph boundary.
	deepInSecondPara := secondPara[200:300]
	if strings.Contains(firstChunk, deepInSecondPara) {
		t.Errorf(
			"first chunk crossed the paragraph boundary and contains content from deep within the second paragraph; "+
				"expected split at \\n\\n boundary, not mid-paragraph; first chunk ends: %q",
			firstChunk[max(0, len(firstChunk)-80):],
		)
	}

	// The second chunk must begin with content from secondPara (possibly
	// after some overlap from the end of firstPara).
	secondChunk := chunks[1]
	if !strings.Contains(secondChunk, secondPara[:50]) {
		t.Errorf("second chunk does not contain the beginning of the second paragraph; "+
			"paragraph boundary split may not have been used; second chunk starts: %q",
			secondChunk[:min(80, len(secondChunk))],
		)
	}
}

// -----------------------------------------------------------------------
// ChunkTextWith: an explicit config drives the chunk geometry. We assert
// contracts (more/smaller chunks, hard cap respected, full coverage, no empty
// chunks) rather than exact sizes, so the test survives chunker tuning.
// -----------------------------------------------------------------------

func TestChunkTextWith_SmallConfigYieldsMoreSmallerChunks(t *testing.T) {
	t.Parallel()

	input := makeText(5000)
	small := ChunkConfig{Target: 300, HardCap: 400, Overlap: 50}

	def := ChunkText(input)
	got := ChunkTextWith(input, small)

	if len(got) <= len(def) {
		t.Errorf("small config produced %d chunks; want more than default's %d", len(got), len(def))
	}
	for i, c := range got {
		if l := len([]rune(c)); l > small.HardCap {
			t.Errorf("chunk[%d] length %d exceeds configured hard cap %d", i, l, small.HardCap)
		}
		if strings.TrimFunc(c, unicode.IsSpace) == "" {
			t.Errorf("chunk[%d] is empty or whitespace-only", i)
		}
	}
}

func TestChunkTextWith_FullCoverageUnderSmallConfig(t *testing.T) {
	t.Parallel()

	input := makeParagraphs(8, 400)
	cfg := ChunkConfig{Target: 350, HardCap: 450, Overlap: 60}
	chunks := ChunkTextWith(input, cfg)
	if len(chunks) == 0 {
		t.Fatal("ChunkTextWith returned no chunks for non-empty input")
	}

	// Every non-whitespace rune in the input must appear across the chunks.
	inputFreq := make(map[rune]int)
	for _, r := range input {
		if !unicode.IsSpace(r) {
			inputFreq[r]++
		}
	}
	chunkFreq := make(map[rune]int)
	for _, c := range chunks {
		for _, r := range c {
			if !unicode.IsSpace(r) {
				chunkFreq[r]++
			}
		}
	}
	for r, n := range inputFreq {
		if chunkFreq[r] < n {
			t.Errorf("rune %q appears %d times in input but only %d across all chunks", r, n, chunkFreq[r])
		}
	}
}

// TestChunkTextWith_NonPositiveFieldsFallBackToDefault asserts the documented
// contract: a zero-valued or all-negative config (and an explicit default) all
// reproduce ChunkText's output exactly.
func TestChunkTextWith_NonPositiveFieldsFallBackToDefault(t *testing.T) {
	t.Parallel()

	input := makeParagraphs(6, 500)
	want := ChunkText(input)

	cases := []struct {
		name string
		cfg  ChunkConfig
	}{
		{"zero value", ChunkConfig{}},
		{"all negative", ChunkConfig{Target: -1, HardCap: -1, Overlap: -1}},
		{"explicit defaults", DefaultChunkConfig()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ChunkTextWith(input, tc.cfg)
			if len(got) != len(want) {
				t.Fatalf("got %d chunks; want %d (should equal ChunkText)", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("chunk[%d] differs from ChunkText output", i)
				}
			}
		})
	}
}

func TestChunkConfig_WithDefaults(t *testing.T) {
	t.Parallel()

	d := DefaultChunkConfig()
	cases := []struct {
		name string
		in   ChunkConfig
		want ChunkConfig
	}{
		{"zero value → all defaults", ChunkConfig{}, d},
		{"only target set", ChunkConfig{Target: 500}, ChunkConfig{Target: 500, HardCap: d.HardCap, Overlap: d.Overlap}},
		{"negative overlap → default overlap", ChunkConfig{Target: 800, HardCap: 1000, Overlap: -5}, ChunkConfig{Target: 800, HardCap: 1000, Overlap: d.Overlap}},
		{"all positive → unchanged", ChunkConfig{Target: 700, HardCap: 900, Overlap: 150}, ChunkConfig{Target: 700, HardCap: 900, Overlap: 150}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.withDefaults(); got != tc.want {
				t.Errorf("withDefaults() = %+v; want %+v", got, tc.want)
			}
		})
	}
}

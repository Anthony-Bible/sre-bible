package eval

import (
	"math"
	"strings"
	"testing"
)

// chunk is a tiny constructor for a RetrievedChunkRecord, keeping the table
// rows below readable.
func chunk(source, content string) RetrievedChunkRecord {
	return RetrievedChunkRecord{SourceName: source, Content: content}
}

func TestExpectedSourceRank(t *testing.T) {
	t.Parallel()

	retrieved := []RetrievedChunkRecord{
		chunk("alpha", "a"),
		chunk("beta", "b"),
		chunk("gamma", "c"),
		chunk("delta", "d"),
	}

	cases := []struct {
		name      string
		expected  []string
		retrieved []RetrievedChunkRecord
		k         int
		want      int
	}{
		{"first slot → rank 1", []string{"alpha"}, retrieved, 8, 1},
		{"third slot → rank 3", []string{"gamma"}, retrieved, 8, 3},
		{"first match of several wins", []string{"gamma", "beta"}, retrieved, 8, 2},
		{"absent → k+1", []string{"omega"}, retrieved, 8, 9},
		{"present but beyond k → k+1", []string{"delta"}, retrieved, 2, 3},
		{"no expectation → 0", nil, retrieved, 8, 0},
		{"empty expected slice → 0", []string{}, retrieved, 8, 0},
		{"no chunks retrieved, expected present → k+1", []string{"alpha"}, nil, 8, 9},
		{"k larger than retrieved, absent → k+1", []string{"omega"}, retrieved, 20, 21},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExpectedSourceRank(tc.expected, tc.retrieved, tc.k); got != tc.want {
				t.Errorf("ExpectedSourceRank(%v, …, k=%d) = %d; want %d", tc.expected, tc.k, got, tc.want)
			}
		})
	}
}

func TestRetrievedContextChars(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		retrieved []RetrievedChunkRecord
		want      int
	}{
		{"empty", nil, 0},
		{"single ascii", []RetrievedChunkRecord{chunk("s", "hello")}, 5},
		// "wörld" is 5 runes but 6 bytes; rune-counting is the contract.
		{"multibyte counted as runes", []RetrievedChunkRecord{chunk("s", "wörld")}, 5},
		{"sum across chunks", []RetrievedChunkRecord{chunk("a", "abc"), chunk("b", "de")}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RetrievedContextChars(tc.retrieved); got != tc.want {
				t.Errorf("RetrievedContextChars(%v) = %d; want %d", tc.retrieved, got, tc.want)
			}
		})
	}
}

// scored is a tiny constructor for a ScoredResult with just the fields the
// diagnostics read: the case's expected sources and the retrieved chunks.
func scored(expected []string, retrieved []RetrievedChunkRecord) ScoredResult {
	return ScoredResult{
		Result: Result{
			Case:            GoldenCase{ExpectedSourceNames: expected},
			RetrievedChunks: retrieved,
		},
	}
}

func TestComputeDiagnostics(t *testing.T) {
	t.Parallel()

	k := 8
	scoredResults := []ScoredResult{
		// expected "alpha" at rank 1, ctx = 3+3 = 6 runes.
		scored([]string{"alpha"}, []RetrievedChunkRecord{chunk("alpha", "abc"), chunk("beta", "def")}),
		// expected "zzz" absent → rank k+1 = 9, ctx = 4 runes.
		scored([]string{"zzz"}, []RetrievedChunkRecord{chunk("beta", "wxyz")}),
		// no expectation → contributes to ctx (3 runes) but NOT to rank.
		scored(nil, []RetrievedChunkRecord{chunk("gamma", "ghi")}),
	}

	d := ComputeDiagnostics(scoredResults, k)

	if d.RankCases != 2 {
		t.Errorf("RankCases = %d; want 2 (only cases declaring expected sources)", d.RankCases)
	}
	wantRank := float64(1+9) / 2.0 // 5.0
	if !almostEqual(d.MeanRank, wantRank) {
		t.Errorf("MeanRank = %v; want %v", d.MeanRank, wantRank)
	}
	wantCtx := float64(6+4+3) / 3.0 // 13/3
	if !almostEqual(d.MeanCtxChars, wantCtx) {
		t.Errorf("MeanCtxChars = %v; want %v", d.MeanCtxChars, wantCtx)
	}
}

func TestComputeDiagnostics_Empty(t *testing.T) {
	t.Parallel()

	d := ComputeDiagnostics(nil, 8)
	if d.RankCases != 0 || d.MeanRank != 0 || d.MeanCtxChars != 0 {
		t.Errorf("empty diagnostics = %+v; want all zero", d)
	}
}

func TestScoreFor(t *testing.T) {
	t.Parallel()

	reports := []CategoryReport{
		{Category: CategoryGroundedFactual, AvgScore: 0.91},
		{Category: CategoryRefusal, AvgScore: 1.0},
	}
	if got := ScoreFor(reports, CategoryGroundedFactual); !almostEqual(got, 0.91) {
		t.Errorf("ScoreFor(grounded) = %v; want 0.91", got)
	}
	if got := ScoreFor(reports, CategoryContactFlow); got != 0 {
		t.Errorf("ScoreFor(absent category) = %v; want 0", got)
	}
}

func TestFormatSweepTable(t *testing.T) {
	t.Parallel()

	rows := []SweepRow{
		{
			Label: "baseline", Target: 1000, HardCap: 1200, Overlap: 200, K: 8,
			Groundedness: 0.912, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0,
			MeanRank: 1.25, MeanCtxChars: 4200, ChunkCount: 40, MedianChunk: 980, P90Chunk: 1150,
		},
		{
			Label: "smaller", Target: 700, HardCap: 900, Overlap: 150, K: 12,
			Groundedness: 0.880, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0,
			MeanRank: 1.50, MeanCtxChars: 5600, ChunkCount: 58, MedianChunk: 690, P90Chunk: 860,
		},
	}
	out := FormatSweepTable(rows)

	// Header + separator + one row per SweepRow.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2+len(rows) {
		t.Fatalf("table has %d lines; want %d\n%s", len(lines), 2+len(rows), out)
	}
	if !strings.Contains(lines[0], "config") || !strings.Contains(lines[0], "mean_rank") {
		t.Errorf("header row missing expected columns: %q", lines[0])
	}
	if !strings.Contains(out, "| baseline |") || !strings.Contains(out, "| smaller |") {
		t.Errorf("table missing a config label row:\n%s", out)
	}
	// Groundedness is rendered to three decimals.
	if !strings.Contains(out, "0.912") {
		t.Errorf("baseline groundedness not rendered at .3f precision:\n%s", out)
	}
}

func TestRecommendConfig(t *testing.T) {
	t.Parallel()

	const tol = 0.05

	baseline := SweepRow{Label: "baseline", Groundedness: 0.90, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0}

	t.Run("baseline wins when candidates within noise", func(t *testing.T) {
		t.Parallel()
		rows := []SweepRow{
			baseline,
			{Label: "smaller", Groundedness: 0.92, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0}, // +0.02 < tol
		}
		got, rationale := RecommendConfig(rows, "baseline", tol)
		if got.Label != "baseline" {
			t.Errorf("recommended %q; want baseline (improvement within noise)", got.Label)
		}
		if !strings.Contains(rationale, "no tuning needed") {
			t.Errorf("rationale = %q; want it to state no tuning needed", rationale)
		}
	})

	t.Run("clear winner beyond tolerance", func(t *testing.T) {
		t.Parallel()
		rows := []SweepRow{
			baseline,
			{Label: "larger", Groundedness: 0.98, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0}, // +0.08 > tol
		}
		got, rationale := RecommendConfig(rows, "baseline", tol)
		if got.Label != "larger" {
			t.Errorf("recommended %q; want larger (improvement beyond noise)", got.Label)
		}
		if !strings.Contains(rationale, "larger") || !strings.Contains(rationale, "improves") {
			t.Errorf("rationale = %q; want it to name the winning config and its improvement", rationale)
		}
	})

	t.Run("guardrail regression disqualifies a higher-groundedness config", func(t *testing.T) {
		t.Parallel()
		rows := []SweepRow{
			baseline,
			// Higher groundedness, but recall craters below baseline-tol → rejected.
			{Label: "risky", Groundedness: 0.99, Recall: 0.70, Refusal: 1.0, ContactFlow: 1.0},
		}
		got, _ := RecommendConfig(rows, "baseline", tol)
		if got.Label != "baseline" {
			t.Errorf("recommended %q; want baseline (candidate regressed recall guardrail)", got.Label)
		}
	})

	t.Run("empty rows", func(t *testing.T) {
		t.Parallel()
		got, rationale := RecommendConfig(nil, "baseline", tol)
		if got.Label != "" {
			t.Errorf("recommended %+v; want zero SweepRow for empty input", got)
		}
		if rationale == "" {
			t.Error("rationale is empty; want a no-rows explanation")
		}
	})

	t.Run("missing baseline falls back to first row", func(t *testing.T) {
		t.Parallel()
		rows := []SweepRow{
			{Label: "first", Groundedness: 0.90, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0},
			{Label: "second", Groundedness: 0.99, Recall: 1.0, Refusal: 1.0, ContactFlow: 1.0},
		}
		got, _ := RecommendConfig(rows, "nonexistent", tol)
		if got.Label != "second" {
			t.Errorf("recommended %q; want second (best vs first-row fallback baseline)", got.Label)
		}
	})
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

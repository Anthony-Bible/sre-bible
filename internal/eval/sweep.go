package eval

import (
	"fmt"
	"strings"
)

// ExpectedSourceRank returns the 1-based position of the first retrieved chunk
// whose source name is one of the expected names, scanning at most the first k
// chunks (retrieved is assumed to be in retrieval order, best first). It returns
// k+1 when no expected source appears within the first k chunks — a sentinel
// "worse than the last slot" value — and 0 when expected is empty (no
// expectation; callers should skip such cases when averaging).
//
// Unlike recall@k, which saturates at 1.0 on a small corpus where the expected
// source is almost always retrieved somewhere in the top-k, rank is a continuous
// signal: it still discriminates configs by *how high* the expected source
// ranks. Lower is better.
func ExpectedSourceRank(expected []string, retrieved []RetrievedChunkRecord, k int) int {
	if len(expected) == 0 {
		return 0
	}
	exp := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		exp[e] = struct{}{}
	}
	limit := len(retrieved)
	if k > 0 && k < limit {
		limit = k
	}
	for i := range limit {
		if _, ok := exp[retrieved[i].SourceName]; ok {
			return i + 1
		}
	}
	return k + 1
}

// RetrievedContextChars returns the total number of runes across all retrieved
// chunk contents — the size of the context window handed to the generator and
// judge. As chunk size and k change this surfaces the precision↔recall tradeoff:
// bigger chunks or larger k feed more context (more recall, more noise/cost).
func RetrievedContextChars(retrieved []RetrievedChunkRecord) int {
	total := 0
	for _, c := range retrieved {
		total += len([]rune(c.Content))
	}
	return total
}

// SweepDiagnostics holds the sweep-only retrieval diagnostics aggregated over
// one config's scored results.
type SweepDiagnostics struct {
	MeanRank     float64 // mean ExpectedSourceRank over cases that declare expected sources (lower = better)
	MeanCtxChars float64 // mean RetrievedContextChars over cases that retrieved at least one chunk
	RankCases    int     // number of cases that contributed to MeanRank
}

// ComputeDiagnostics aggregates ExpectedSourceRank and RetrievedContextChars
// over scored results. k is the retrieval depth used for the run (the rank
// sentinel ceiling). MeanRank averages only over cases that declare expected
// sources; MeanCtxChars averages over cases that retrieved at least one chunk.
func ComputeDiagnostics(scored []ScoredResult, k int) SweepDiagnostics {
	var rankSum, rankN, ctxSum, ctxN int
	for _, sr := range scored {
		retrieved := sr.Result.RetrievedChunks
		if expected := sr.Result.Case.ExpectedSourceNames; len(expected) > 0 {
			rankSum += ExpectedSourceRank(expected, retrieved, k)
			rankN++
		}
		if len(retrieved) > 0 {
			ctxSum += RetrievedContextChars(retrieved)
			ctxN++
		}
	}
	var d SweepDiagnostics
	d.RankCases = rankN
	if rankN > 0 {
		d.MeanRank = float64(rankSum) / float64(rankN)
	}
	if ctxN > 0 {
		d.MeanCtxChars = float64(ctxSum) / float64(ctxN)
	}
	return d
}

// ScoreFor returns the AvgScore of the named category among reports, or 0 when
// the category is absent. A small accessor so the sweep can flatten the
// []CategoryReport that Aggregate returns into a SweepRow.
func ScoreFor(reports []CategoryReport, cat Category) float64 {
	for _, r := range reports {
		if r.Category == cat {
			return r.AvgScore
		}
	}
	return 0
}

// SweepRow is one row of the chunking-sweep scorecard: a single config's
// four category scores plus the sweep-only diagnostics and chunk-size shape.
type SweepRow struct {
	Label        string
	Target       int
	HardCap      int
	Overlap      int
	K            int
	Groundedness float64
	Recall       float64
	Refusal      float64
	ContactFlow  float64
	ToolFlow     float64
	MeanRank     float64
	MeanCtxChars float64
	ChunkCount   int // total chunks stored in the DB under this config
	MedianChunk  int // median chunk size in chars
	P90Chunk     int // 90th-percentile chunk size in chars
}

// FormatSweepTable renders rows as a GitHub-flavored markdown table. The column
// order matches the config grid then the metrics, so the baseline row reads
// left-to-right as the knobs that produced the scores to its right.
func FormatSweepTable(rows []SweepRow) string {
	var b strings.Builder
	b.WriteString("| config | target | hardCap | overlap | k | ground | recall | refusal | contact | tool_flow | mean_rank | mean_ctx_chars | chunks | median | p90 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.3f | %.3f | %.3f | %.3f | %.3f | %.2f | %.0f | %d | %d | %d |\n",
			r.Label, r.Target, r.HardCap, r.Overlap, r.K,
			r.Groundedness, r.Recall, r.Refusal, r.ContactFlow, r.ToolFlow,
			r.MeanRank, r.MeanCtxChars, r.ChunkCount, r.MedianChunk, r.P90Chunk)
	}
	return b.String()
}

// RecommendConfig picks the row with the best groundedness that does not regress
// the recall/refusal/contact/tool_flow guardrails below the baseline row's values
// by more than tol, and reports a one-line rationale. The baseline always
// qualifies (it cannot regress against itself), so a recommendation is always
// returned when rows is non-empty and contains baselineLabel.
//
// The decision is deliberately conservative: a candidate only wins if it beats
// baseline groundedness by more than tol (LLM-judge noise) AND holds every
// guardrail. Otherwise the rationale states plainly that no tuning is warranted —
// the honest answer when the knobs don't move the needle on this corpus.
func RecommendConfig(rows []SweepRow, baselineLabel string, tol float64) (SweepRow, string) {
	var base SweepRow
	var haveBase bool
	for _, r := range rows {
		if r.Label == baselineLabel {
			base, haveBase = r, true
			break
		}
	}
	if !haveBase {
		if len(rows) == 0 {
			return SweepRow{}, "no rows to evaluate"
		}
		// Fall back to the first row as the reference point.
		base = rows[0]
		baselineLabel = base.Label
	}

	regresses := func(r SweepRow) bool {
		return r.Recall < base.Recall-tol ||
			r.Refusal < base.Refusal-tol ||
			r.ContactFlow < base.ContactFlow-tol ||
			r.ToolFlow < base.ToolFlow-tol
	}

	best := base
	for _, r := range rows {
		if r.Label == baselineLabel || regresses(r) {
			continue
		}
		if r.Groundedness > best.Groundedness {
			best = r
		}
	}

	if best.Label == baselineLabel || best.Groundedness <= base.Groundedness+tol {
		return base, fmt.Sprintf(
			"no tuning needed: baseline %q (ground %.3f) is within judge noise (±%.3f) of every non-regressing config; the knobs don't move the needle on this corpus",
			baselineLabel, base.Groundedness, tol)
	}
	return best, fmt.Sprintf(
		"config %q improves groundedness %.3f → %.3f (+%.3f, above ±%.3f noise) without regressing recall/refusal/contact/tool_flow below baseline",
		best.Label, base.Groundedness, best.Groundedness, best.Groundedness-base.Groundedness, tol)
}

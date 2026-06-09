package eval

import "log/slog"

// Thresholds holds the minimum acceptable score for each category gate.
type Thresholds struct {
	Groundedness float64
	Recall       float64
	Refusal      float64
	ContactFlow  float64
}

// DefaultThresholds is the gate configuration, set from a baseline run of the
// golden dataset against the ingested fixtures (see ADR 0010). Observed
// per-category scores were 1.00 across the board (groundedness, recall, refusal,
// contact_flow). Recall is measured over a corpus that includes distractor
// documents, so a 1.00 means retrieval preferred the expected source over
// topical look-alikes, not that there was nothing else to retrieve. The recall,
// refusal, and contact gates sit just below their observed 1.00 so they catch
// regressions without flaking on the residual variance an LLM-judged pipeline
// carries even with the agent pinned to temperature=0. Groundedness keeps a
// wider precautionary margin (0.75 vs 1.00): the gf-004 retrieval-coverage
// artifact that once docked one case is resolved by the chunking fix, so the
// margin now just tolerates a single grounded case regressing without flaking.
// Re-baseline when the fixture set, the chunker, the judge model, the agent
// model, or the agent temperature changes.
var DefaultThresholds = Thresholds{
	Groundedness: 0.75,
	Recall:       0.80,
	Refusal:      0.90,
	ContactFlow:  0.80,
}

// CategoryReport is the aggregated pass/fail summary for one Category.
type CategoryReport struct {
	Category  Category
	Total     int
	Passed    int
	AvgScore  float64
	Threshold float64
	MeetsGate bool
}

// Aggregate groups ScoredResults by category and computes per-category metrics.
// Each category uses the threshold from thresholds:
//
//	grounded_factual  → avg GroundScore (skip JudgeSkipped entries)
//	retrieval_check   → avg RecallScore (skip -1 entries)
//	refusal           → pass rate (Passed/Total)
//	contact_flow      → pass rate (Passed/Total)
//
// Reports are returned in the fixed order: grounded_factual, retrieval_check,
// refusal, contact_flow (matching the Category constants declaration order).
func Aggregate(results []ScoredResult, thresholds Thresholds) []CategoryReport {
	type accum struct {
		total    int
		passed   int
		scoreSum float64
		scoreN   int
	}

	acc := map[Category]*accum{
		CategoryGroundedFactual: {},
		CategoryRetrievalCheck:  {},
		CategoryRefusal:         {},
		CategoryContactFlow:     {},
	}

	for _, sr := range results {
		cat := sr.Result.Case.Category
		a, ok := acc[cat]
		if !ok {
			continue
		}
		a.total++
		if sr.Pass {
			a.passed++
		}
		switch cat {
		case CategoryGroundedFactual:
			if !sr.Score.JudgeSkipped {
				a.scoreSum += sr.Score.GroundScore
				a.scoreN++
			}
		case CategoryRetrievalCheck:
			if sr.Score.RecallScore >= 0 {
				a.scoreSum += sr.Score.RecallScore
				a.scoreN++
			}
		case CategoryRefusal, CategoryContactFlow:
			// avg score = pass rate; computed below.
		}
	}

	thresholdFor := map[Category]float64{
		CategoryGroundedFactual: thresholds.Groundedness,
		CategoryRetrievalCheck:  thresholds.Recall,
		CategoryRefusal:         thresholds.Refusal,
		CategoryContactFlow:     thresholds.ContactFlow,
	}

	order := []Category{
		CategoryGroundedFactual,
		CategoryRetrievalCheck,
		CategoryRefusal,
		CategoryContactFlow,
	}

	reports := make([]CategoryReport, 0, len(order))
	for _, cat := range order {
		a := acc[cat]
		thresh := thresholdFor[cat]

		var avgScore float64
		switch cat {
		case CategoryGroundedFactual, CategoryRetrievalCheck:
			if a.scoreN > 0 {
				avgScore = a.scoreSum / float64(a.scoreN)
			}
		case CategoryRefusal, CategoryContactFlow:
			if a.total > 0 {
				avgScore = float64(a.passed) / float64(a.total)
			}
		}

		meetsGate := a.total == 0 || avgScore >= thresh

		reports = append(reports, CategoryReport{
			Category:  cat,
			Total:     a.total,
			Passed:    a.passed,
			AvgScore:  avgScore,
			Threshold: thresh,
			MeetsGate: meetsGate,
		})
	}

	return reports
}

// Report logs each CategoryReport and returns true only if every category
// meets its gate. A false return signals that the eval pipeline should fail.
func Report(reports []CategoryReport, log *slog.Logger) bool {
	allPass := true
	for _, r := range reports {
		log.Info("eval category report",
			"category", string(r.Category),
			"total", r.Total,
			"passed", r.Passed,
			"avg_score", r.AvgScore,
			"threshold", r.Threshold,
			"meets_gate", r.MeetsGate,
		)
		if !r.MeetsGate {
			allPass = false
		}
	}
	return allPass
}

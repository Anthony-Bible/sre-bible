package eval_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/eval"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// ---------------------------------------------------------------------------
// Stub implementations
// ---------------------------------------------------------------------------

type stubEmbedder struct{}

func (stubEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

type stubSearcher struct {
	chunks []rag.RetrievedChunk
}

func (s stubSearcher) SearchChunks(_ context.Context, _ []float32, _ int) ([]rag.RetrievedChunk, error) {
	return s.chunks, nil
}

type stubGenerator struct {
	tokens       []string
	traceSteps   []rag.TraceStep
	fetchedNames []string
}

func (g *stubGenerator) StreamAnswer(
	_ context.Context,
	_ []rag.Message,
	_ rag.ToolSet,
	onToken func(string) error,
	onTrace func(rag.TraceStep) error,
) ([]string, error) {
	for _, step := range g.traceSteps {
		if onTrace != nil {
			if err := onTrace(step); err != nil {
				return nil, err
			}
		}
	}
	for _, tok := range g.tokens {
		if err := onToken(tok); err != nil {
			return nil, err
		}
	}
	return g.fetchedNames, nil
}

// stubJudge returns a configurable verdict or error. refusalCalls records how
// many times IsRefusal was invoked so tests can assert the (cost-sensitive)
// refusal judge only fires for cases that expect a refusal.
type stubJudge struct {
	verdict      eval.JudgeVerdict
	err          error
	refused      bool
	refusalErr   error
	refusalCalls int
}

func (j *stubJudge) Score(_ context.Context, _, _, _, _ string) (eval.JudgeVerdict, error) {
	return j.verdict, j.err
}

func (j *stubJudge) IsRefusal(_ context.Context, _, _ string) (bool, error) {
	j.refusalCalls++
	return j.refused, j.refusalErr
}

// newRunner is a test helper that builds a Runner with the given searcher and generator.
func newRunner(searcher rag.ChunkSearcher, gen rag.Generator, judge eval.Judge) *eval.Runner {
	return eval.NewRunner(
		stubEmbedder{},
		searcher,
		gen,
		nil, // lister
		nil, // fetcher
		judge,
		slog.Default(),
	)
}

// ---------------------------------------------------------------------------
// Runner.Run tests
// ---------------------------------------------------------------------------

func TestRunner_Run_PopulatesAnswer(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "some context", SourceName: "resume.pdf"}}
	gen := &stubGenerator{tokens: []string{"Hello", " world"}}
	r := newRunner(stubSearcher{chunks: chunks}, gen, nil)

	result := r.Run(context.Background(), eval.GoldenCase{
		ID:       "tc-001",
		Category: eval.CategoryGroundedFactual,
		Question: "What did Anthony do?",
	})

	if result.Error != nil {
		t.Fatalf("Run: unexpected error: %v", result.Error)
	}
	if result.Answer != "Hello world" {
		t.Errorf("Answer: got %q, want %q", result.Answer, "Hello world")
	}
}

func TestRunner_Run_CapturesToolCalls(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	toolStep := rag.TraceStep{
		Kind:     rag.TraceKindToolCall,
		Label:    "Listing docs",
		ToolCall: &rag.ToolCallDetail{Tool: "list_documents", Outcome: "ok"},
	}
	gen := &stubGenerator{
		tokens:     []string{"answer"},
		traceSteps: []rag.TraceStep{toolStep},
	}
	r := newRunner(stubSearcher{chunks: chunks}, gen, nil)

	result := r.Run(context.Background(), eval.GoldenCase{
		ID:                "tc-002",
		Category:          eval.CategoryContactFlow,
		Question:          "Can you send a message?",
		ExpectedToolCalls: []string{"list_documents"},
	})

	if result.Error != nil {
		t.Fatalf("Run: unexpected error: %v", result.Error)
	}
	if len(result.ToolCallsSeen) == 0 {
		t.Fatal("ToolCallsSeen must not be empty when a tool_call trace step was emitted")
	}
	found := false
	for _, tc := range result.ToolCallsSeen {
		if tc == "list_documents" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ToolCallsSeen %v does not contain 'list_documents'", result.ToolCallsSeen)
	}
}

// ---------------------------------------------------------------------------
// Runner.Score tests
// ---------------------------------------------------------------------------

func TestRunner_Score_PassesWhenRefusalExpectedAndDetected(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:              "tc-003",
			Category:        eval.CategoryRefusal,
			Question:        "Tell me a joke",
			ExpectedRefusal: true,
		},
		Answer: "I couldn't find relevant information in my knowledge base to answer that question.",
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if !sr.Score.RefusalPass {
		t.Error("RefusalPass should be true when refusal is expected and answer contains refusal signal")
	}
	if !sr.Pass {
		t.Errorf("Pass should be true; Notes: %s", sr.Notes)
	}
}

func TestRunner_Score_FailsWhenRefusalExpectedButNotDetected(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:              "tc-004",
			Category:        eval.CategoryRefusal,
			Question:        "What is SRE?",
			ExpectedRefusal: true,
		},
		Answer: "SRE stands for Site Reliability Engineering. Anthony has extensive SRE experience.",
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.RefusalPass {
		t.Error("RefusalPass should be false when refusal expected but answer is substantive")
	}
	if sr.Pass {
		t.Error("Pass should be false when refusal check fails")
	}
}

func TestRunner_Score_UsesJudgeForRefusalWhenConfigured(t *testing.T) {
	t.Parallel()

	// The agent declines with wording the keyword heuristic does not match (the
	// CI false-negative case: a tailored PII redirect, not the off-topic
	// sentinel). With a judge configured, its semantic verdict (refused=true)
	// must make RefusalPass true even though RefusalCorrect alone would miss it.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:              "tc-refusal-judge",
			Category:        eval.CategoryRefusal,
			Question:        "What is Anthony's email address?",
			ExpectedRefusal: true,
		},
		Answer: "I can't share Anthony's email directly, but you can reach him on LinkedIn.",
	}

	j := &stubJudge{refused: true}
	r := newRunner(stubSearcher{}, &stubGenerator{}, j)
	sr := r.Score(context.Background(), result)

	if j.refusalCalls != 1 {
		t.Errorf("refusal judge consulted %d times; want 1", j.refusalCalls)
	}
	if !sr.Score.RefusalPass {
		t.Error("RefusalPass should be true: judge classifies the non-sentinel answer as a refusal")
	}
}

func TestRunner_Score_RefusalJudgeErrorFallsBackToHeuristic(t *testing.T) {
	t.Parallel()

	// When the refusal judge errors, scoring falls back to the deterministic
	// keyword heuristic rather than crashing the gate. This answer carries no
	// sentinel phrase, so the heuristic reports not-a-refusal and the expected
	// refusal is (visibly) unsatisfied.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:              "tc-refusal-fallback",
			Category:        eval.CategoryRefusal,
			Question:        "What is Anthony's phone number?",
			ExpectedRefusal: true,
		},
		Answer: "I can't share personal contact details.",
	}

	j := &stubJudge{refusalErr: errors.New("judge API unavailable")}
	r := newRunner(stubSearcher{}, &stubGenerator{}, j)
	sr := r.Score(context.Background(), result)

	if sr.Score.RefusalPass {
		t.Error("RefusalPass should be false: judge errored and the keyword heuristic finds no refusal signal")
	}
}

func TestRunner_Score_DoesNotConsultRefusalJudgeWhenNotExpected(t *testing.T) {
	t.Parallel()

	// Cost guard: the refusal judge is an LLM call and must only fire for cases
	// that expect a refusal. A grounded_factual case (ExpectedRefusal=false) must
	// take the cheap heuristic path and never consult the judge — even one
	// configured to (wrongly) report a refusal.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:       "tc-no-refusal",
			Category: eval.CategoryGroundedFactual,
			Question: "What did Anthony do at Acme?",
		},
		Answer: "Anthony led SRE at Acme and improved uptime.",
	}

	j := &stubJudge{refused: true}
	r := newRunner(stubSearcher{}, &stubGenerator{}, j)
	sr := r.Score(context.Background(), result)

	if j.refusalCalls != 0 {
		t.Errorf("refusal judge consulted %d times for a non-refusal case; want 0", j.refusalCalls)
	}
	if !sr.Score.RefusalPass {
		t.Error("RefusalPass should be true: no refusal expected and the answer is substantive")
	}
}

func TestRunner_Score_FailsWhenMustNotContainViolated(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:             "tc-005",
			Category:       eval.CategoryGroundedFactual,
			Question:       "What is Anthony's salary?",
			MustNotContain: []string{"$", "salary"},
		},
		Answer: "Anthony's salary is $200,000.",
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.MustNotPass {
		t.Error("MustNotPass should be false when a forbidden substring appears in the answer")
	}
	if sr.Pass {
		t.Error("Pass should be false when must-not-contain check fails")
	}
}

func TestRunner_Score_UsesStubJudgeScore(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:          "tc-006",
			Category:    eval.CategoryGroundedFactual,
			Question:    "Describe Anthony's SRE experience",
			JudgeRubric: "Answer must cite specific reliability achievements.",
		},
		Answer:          "Anthony improved uptime by 99.9% at Acme Corp.",
		RetrievedChunks: []eval.RetrievedChunkRecord{{Content: "uptime context", SourceName: "resume.pdf"}},
	}

	j := &stubJudge{verdict: eval.JudgeVerdict{Score: 0.9, Rationale: "Well grounded"}}
	r := newRunner(stubSearcher{}, &stubGenerator{}, j)
	sr := r.Score(context.Background(), result)

	if sr.Score.JudgeSkipped {
		t.Error("JudgeSkipped should be false when judge succeeds")
	}
	if sr.Score.GroundScore != 0.9 {
		t.Errorf("GroundScore: got %f, want 0.9", sr.Score.GroundScore)
	}
}

func TestRunner_Score_SkipsJudgeOnError(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:          "tc-007",
			Category:    eval.CategoryGroundedFactual,
			Question:    "Describe Anthony's SRE experience",
			JudgeRubric: "Answer must cite specific reliability achievements.",
		},
		Answer: "Anthony worked at Acme Corp.",
	}

	j := &stubJudge{err: errors.New("judge API unavailable")}
	r := newRunner(stubSearcher{}, &stubGenerator{}, j)
	sr := r.Score(context.Background(), result)

	if !sr.Score.JudgeSkipped {
		t.Error("JudgeSkipped should be true when judge returns an error")
	}
	if sr.Score.GroundScore != -1 {
		t.Errorf("GroundScore should remain -1 when judge is skipped, got %f", sr.Score.GroundScore)
	}
}

func TestRunner_Score_FailsWhenMustContainMissing(t *testing.T) {
	t.Parallel()

	// A contact_flow case that requires the answer to mention "linkedin". The
	// answer omits it, so MustContainPass is false and the case fails — even
	// though no other assertion is violated.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:          "tc-must-contain",
			Category:    eval.CategoryContactFlow,
			Question:    "How do I reach Anthony?",
			MustContain: []string{"linkedin"},
		},
		Answer: "You can send him a message through the website.",
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.MustContainPass {
		t.Error("MustContainPass should be false when a required substring is absent")
	}
	if sr.Pass {
		t.Error("Pass should be false when must-contain check fails")
	}
}

func TestRunner_Score_PassesWhenMustContainPresent(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:          "tc-must-contain-ok",
			Category:    eval.CategoryContactFlow,
			Question:    "How do I reach Anthony?",
			MustContain: []string{"linkedin"},
		},
		Answer: "The best way is via LinkedIn at linkedin.com/in/anthonybible/.",
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if !sr.Score.MustContainPass {
		t.Error("MustContainPass should be true when all required substrings are present (case-insensitive)")
	}
	if !sr.Pass {
		t.Errorf("Pass should be true; Notes: %s", sr.Notes)
	}
}

func TestRunner_Score_CitationScorePopulatedAndPasses(t *testing.T) {
	t.Parallel()

	// The case declares an expected citation that the pipeline actually returned,
	// so CitationScore is 1.0 (>= the majority bar) and the case passes.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:                "tc-citation-ok",
			Category:          eval.CategoryRetrievalCheck,
			Question:          "What is Anthony's toil philosophy?",
			ExpectedCitations: []string{"about_fixture.txt"},
		},
		Answer:    "He automates repeated toil.",
		Citations: []string{"about_fixture.txt", "resume_fixture.txt"},
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.CitationScore != 1.0 {
		t.Errorf("CitationScore: got %v, want 1.0", sr.Score.CitationScore)
	}
	if !sr.Pass {
		t.Errorf("Pass should be true when the expected citation is present; Notes: %s", sr.Notes)
	}
}

func TestRunner_Score_FailsWhenExpectedCitationMissing(t *testing.T) {
	t.Parallel()

	// Expected citation is absent from the returned set → CitationScore 0.0,
	// below the 0.5 majority bar, so the case fails.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:                "tc-citation-miss",
			Category:          eval.CategoryRetrievalCheck,
			Question:          "What is Anthony's toil philosophy?",
			ExpectedCitations: []string{"about_fixture.txt"},
		},
		Answer:    "He automates repeated toil.",
		Citations: []string{"resume_fixture.txt"},
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.CitationScore != 0.0 {
		t.Errorf("CitationScore: got %v, want 0.0", sr.Score.CitationScore)
	}
	if sr.Pass {
		t.Error("Pass should be false when the expected citation is missing")
	}
}

func TestRunner_Score_CitationScoreSkippedWhenNoneExpected(t *testing.T) {
	t.Parallel()

	// No expected citations declared → CitationScore -1 (skip); the absence of
	// citations never trips the gate.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:       "tc-citation-skip",
			Category: eval.CategoryGroundedFactual,
			Question: "What did Anthony do?",
		},
		Answer:    "He built an agentic RCA system.",
		Citations: []string{},
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.CitationScore != -1 {
		t.Errorf("CitationScore: got %v, want -1 (skip)", sr.Score.CitationScore)
	}
	if !sr.Pass {
		t.Errorf("Pass should be true when no citations are expected; Notes: %s", sr.Notes)
	}
}

func TestRunner_Score_ToolCallsPassReflectsSeenTools(t *testing.T) {
	t.Parallel()

	// tool_flow-style case: the expected tool was invoked, so ToolCallsPass is
	// true and the deterministic gate is satisfied.
	result := eval.Result{
		Case: eval.GoldenCase{
			ID:                "tc-tool-flow",
			Category:          eval.CategoryToolFlow,
			Question:          "Match this job description.",
			ExpectedToolCalls: []string{"match_job_description"},
		},
		Answer:        "Here is the fit scorecard.",
		ToolCallsSeen: []string{"match_job_description"},
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if !sr.Score.ToolCallsPass {
		t.Error("ToolCallsPass should be true when the expected tool was invoked")
	}
	if !sr.Pass {
		t.Errorf("Pass should be true; Notes: %s", sr.Notes)
	}
}

func TestRunner_Score_FailsWhenExpectedToolCallMissing(t *testing.T) {
	t.Parallel()

	result := eval.Result{
		Case: eval.GoldenCase{
			ID:                "tc-tool-flow-miss",
			Category:          eval.CategoryToolFlow,
			Question:          "Match this job description.",
			ExpectedToolCalls: []string{"match_job_description"},
		},
		Answer:        "Anthony is a great fit.",
		ToolCallsSeen: []string{},
	}

	r := newRunner(stubSearcher{}, &stubGenerator{}, nil)
	sr := r.Score(context.Background(), result)

	if sr.Score.ToolCallsPass {
		t.Error("ToolCallsPass should be false when the expected tool was not invoked")
	}
	if sr.Pass {
		t.Error("Pass should be false when a required tool call is missing")
	}
}

// ---------------------------------------------------------------------------
// Report tests
// ---------------------------------------------------------------------------

// cleanScore is a ScoreDetail with every deterministic-assertion gate satisfied:
// the must-pass booleans true and CitationScore -1 (no expected citations). A
// report test builds on this so the hard gate doesn't flunk an otherwise-passing
// category — the zero value of CitationScore (0.0) would otherwise read as a
// citation regression (< 0.5) and trip the gate.
func cleanScore() eval.ScoreDetail {
	return eval.ScoreDetail{
		MustNotPass:     true,
		MustContainPass: true,
		ToolCallsPass:   true,
		CitationScore:   -1,
	}
}

func TestReport_AllPassWhenAllAboveThreshold(t *testing.T) {
	t.Parallel()

	// One passing ScoredResult per category, each with a soft-gate score above
	// default thresholds and no hard-gate violation.
	grounded := cleanScore()
	grounded.GroundScore = 0.95
	retrieval := cleanScore()
	retrieval.RecallScore = 0.90
	refusal := cleanScore()
	refusal.RefusalPass = true
	contact := cleanScore()
	contact.RefusalPass = true

	results := []eval.ScoredResult{
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryGroundedFactual}}, Score: grounded, Pass: true},
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRetrievalCheck}}, Score: retrieval, Pass: true},
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRefusal}}, Score: refusal, Pass: true},
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryContactFlow}}, Score: contact, Pass: true},
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryToolFlow}}, Score: cleanScore(), Pass: true},
	}

	reports := eval.Aggregate(results, eval.DefaultThresholds)
	ok := eval.Report(reports, slog.Default())
	if !ok {
		t.Error("Report should return true when all categories meet their gate")
	}
}

func TestReport_HardFailFlunksCategoryDespiteHighAverage(t *testing.T) {
	t.Parallel()

	// Two grounded_factual cases, both with judge scores well above the
	// groundedness threshold — so the soft (average) gate is satisfied. But one
	// case violated a deterministic assertion (a must_not_contain leak). The hard
	// gate must flunk the whole category regardless of the high average.
	good := cleanScore()
	good.GroundScore = 0.99
	leaked := cleanScore()
	leaked.GroundScore = 0.99
	leaked.MustNotPass = false // e.g. a PII string slipped through

	results := []eval.ScoredResult{
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryGroundedFactual}}, Score: good, Pass: true},
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryGroundedFactual}}, Score: leaked, Pass: false},
	}

	reports := eval.Aggregate(results, eval.DefaultThresholds)
	if len(reports) == 0 {
		t.Fatal("expected at least one category report")
	}
	gf := reports[0]
	if gf.Category != eval.CategoryGroundedFactual {
		t.Fatalf("first report category = %q, want grounded_factual", gf.Category)
	}
	if gf.AvgScore < gf.Threshold {
		t.Fatalf("precondition: average %.2f should clear threshold %.2f so only the hard gate can flunk it", gf.AvgScore, gf.Threshold)
	}
	if !gf.HardFail {
		t.Error("HardFail should be true when a case violates a deterministic assertion")
	}
	if gf.MeetsGate {
		t.Error("MeetsGate should be false: a hard-fail flunks the category even with a high average")
	}

	if eval.Report(reports, slog.Default()) {
		t.Error("Report should return false when a category hard-fails")
	}
}

func TestReport_CitationRegressionHardFails(t *testing.T) {
	t.Parallel()

	// A retrieval_check case whose recall clears the soft gate, but whose expected
	// citation was missed (CitationScore below the majority bar). The citation
	// shortfall is a hard-gate violation, so the category fails.
	s := cleanScore()
	s.RecallScore = 1.0
	s.CitationScore = 0.0 // expected a citation, got none of it

	results := []eval.ScoredResult{
		{Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRetrievalCheck}}, Score: s, Pass: false},
	}

	reports := eval.Aggregate(results, eval.DefaultThresholds)
	rc := reports[1] // retrieval_check is second in the fixed order
	if rc.Category != eval.CategoryRetrievalCheck {
		t.Fatalf("second report category = %q, want retrieval_check", rc.Category)
	}
	if !rc.HardFail {
		t.Error("HardFail should be true when a citation regression is present")
	}
	if rc.MeetsGate {
		t.Error("MeetsGate should be false on a citation hard-fail")
	}
}

func TestReport_FailsWhenAnyScoreBelowThreshold(t *testing.T) {
	t.Parallel()

	// Refusal category with 0% pass rate (below 0.90 threshold).
	results := []eval.ScoredResult{
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRefusal}},
			Score:  eval.ScoreDetail{RefusalPass: false, MustNotPass: true},
			Pass:   false,
		},
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRefusal}},
			Score:  eval.ScoreDetail{RefusalPass: false, MustNotPass: true},
			Pass:   false,
		},
	}

	reports := eval.Aggregate(results, eval.DefaultThresholds)
	ok := eval.Report(reports, slog.Default())
	if ok {
		t.Error("Report should return false when at least one category is below its threshold")
	}
}

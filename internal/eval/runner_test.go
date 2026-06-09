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

// ---------------------------------------------------------------------------
// Report tests
// ---------------------------------------------------------------------------

func TestReport_AllPassWhenAllAboveThreshold(t *testing.T) {
	t.Parallel()

	// Build one passing ScoredResult per category, each with a score above default thresholds.
	results := []eval.ScoredResult{
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryGroundedFactual}},
			Score:  eval.ScoreDetail{GroundScore: 0.95, JudgeSkipped: false},
			Pass:   true,
		},
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRetrievalCheck}},
			Score:  eval.ScoreDetail{RecallScore: 0.90},
			Pass:   true,
		},
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryRefusal}},
			Score:  eval.ScoreDetail{RefusalPass: true, MustNotPass: true},
			Pass:   true,
		},
		{
			Result: eval.Result{Case: eval.GoldenCase{Category: eval.CategoryContactFlow}},
			Score:  eval.ScoreDetail{MustNotPass: true, RefusalPass: true},
			Pass:   true,
		},
	}

	reports := eval.Aggregate(results, eval.DefaultThresholds)
	ok := eval.Report(reports, slog.Default())
	if !ok {
		t.Error("Report should return true when all categories meet their gate")
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

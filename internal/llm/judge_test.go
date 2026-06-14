package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// stubJudge is a fake rag.Judge for testing the evaluate_interview_answer wrapper.
// It records every call and returns a pre-configured evaluation (or error).
type stubJudge struct {
	mu     sync.Mutex
	eval   *rag.InterviewEvaluation
	err    error
	calls  int
	gotIdx int
	gotQ   string
	gotA   string
}

func (s *stubJudge) EvaluateAnswer(_ context.Context, qIdx int, qText, userAnswer string) (*rag.InterviewEvaluation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.gotIdx = qIdx
	s.gotQ = qText
	s.gotA = userAnswer
	if s.err != nil {
		return nil, s.err
	}
	return s.eval, nil
}

func judgeTU(in map[string]any) anthropic.ToolUseBlock {
	raw, _ := json.Marshal(in)
	return anthropic.ToolUseBlock{ID: "j1", Name: toolEvaluateInterviewAnswer, Input: json.RawMessage(raw)}
}

func judgeTURaw(raw string) anthropic.ToolUseBlock {
	return anthropic.ToolUseBlock{ID: "j1", Name: toolEvaluateInterviewAnswer, Input: json.RawMessage(raw)}
}

func parseEval(t *testing.T, text string) rag.InterviewEvaluation {
	t.Helper()
	var got rag.InterviewEvaluation
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("tool result is not valid InterviewEvaluation JSON: %v\nraw: %q", err, text)
	}
	return got
}

// --- buildToolParams gating ---

func TestBuildToolParams_NoJudgeOmitsTool(t *testing.T) {
	t.Parallel()
	params := buildToolParams(rag.ToolSet{Lister: stubLister{}})
	for _, p := range params {
		if p.OfTool != nil && p.OfTool.Name == toolEvaluateInterviewAnswer {
			t.Fatal("evaluate_interview_answer should not be present when Judge is nil")
		}
	}
}

func TestBuildToolParams_WithJudgeIncludesTool(t *testing.T) {
	t.Parallel()
	params := buildToolParams(rag.ToolSet{Judge: &stubJudge{}})
	var tool *anthropic.ToolParam
	for i := range params {
		if params[i].OfTool != nil && params[i].OfTool.Name == toolEvaluateInterviewAnswer {
			tool = params[i].OfTool
		}
	}
	if tool == nil {
		t.Fatal("evaluate_interview_answer should be present when Judge is non-nil")
	}
	wantReq := []string{fieldQuestionIndex, fieldQuestionText, fieldUserAnswer}
	if len(tool.InputSchema.Required) != len(wantReq) {
		t.Fatalf("required: got %v, want %v", tool.InputSchema.Required, wantReq)
	}
	for i, f := range wantReq {
		if tool.InputSchema.Required[i] != f {
			t.Errorf("required[%d]: got %q, want %q", i, tool.InputSchema.Required[i], f)
		}
	}
}

// --- runEvaluateInterviewAnswer: happy paths ---

func TestRunEval_HighQualityAnswer_PassesThroughScore(t *testing.T) {
	t.Parallel()
	j := &stubJudge{eval: &rag.InterviewEvaluation{
		Score:                85,
		Feedback:             "Clear sequencing and singleflight call-out.",
		Passed:               true,
		ConceptsDemonstrated: []string{"singleflight", "circuit breaker"},
	}}
	c := newTestClient()

	text, isErr, sources := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 0,
		"question_text":  "Flash sale stampede.",
		"user_answer":    "Detailed thoughtful answer about singleflight and circuit breakers.",
	}), rag.ToolSet{Judge: j}, nil)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	if len(sources) != 0 {
		t.Errorf("judge tool must not produce document citations, got %v", sources)
	}
	if j.calls != 1 {
		t.Errorf("judge called %d times, want 1", j.calls)
	}
	if j.gotIdx != 0 {
		t.Errorf("judge got idx=%d, want 0", j.gotIdx)
	}
	got := parseEval(t, text)
	if got.Score < 70 {
		t.Errorf("score: got %d, want >=70", got.Score)
	}
	if !got.Passed {
		t.Error("Passed: got false, want true")
	}
}

func TestRunEval_LowEffortAnswer_LowScore(t *testing.T) {
	t.Parallel()
	j := &stubJudge{eval: &rag.InterviewEvaluation{
		Score:    10,
		Feedback: "Low effort response; no scenario engagement.",
		Passed:   false,
	}}
	c := newTestClient()

	text, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 1,
		"question_text":  "BGP/DNS triage.",
		"user_answer":    "idk",
	}), rag.ToolSet{Judge: j}, nil)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	got := parseEval(t, text)
	if got.Score > 20 {
		t.Errorf("score: got %d, want <=20", got.Score)
	}
	if got.Passed {
		t.Error("Passed: got true, want false")
	}
}

// --- runEvaluateInterviewAnswer: input validation ---

func TestRunEval_EmptyAnswer_ErrorWithoutCallingJudge(t *testing.T) {
	t.Parallel()
	j := &stubJudge{}
	c := newTestClient()

	cases := []string{"", "   ", "\n\t"}
	for _, a := range cases {
		_, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
			"question_index": 0,
			"question_text":  "q",
			"user_answer":    a,
		}), rag.ToolSet{Judge: j}, nil)
		if !isErr {
			t.Errorf("answer=%q: expected isErr=true", a)
		}
	}
	if j.calls != 0 {
		t.Errorf("judge must not be called for empty answers, got %d calls", j.calls)
	}
}

func TestRunEval_MalformedQuestionIndex(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	j := &stubJudge{eval: &rag.InterviewEvaluation{Score: 50}}

	cases := []struct {
		name string
		raw  string
	}{
		{"out-of-range-high", `{"question_index": 3, "question_text": "q", "user_answer": "a"}`},
		{"out-of-range-neg", `{"question_index": -1, "question_text": "q", "user_answer": "a"}`},
		{"missing", `{"question_text": "q", "user_answer": "a"}`},
		{"non-int", `{"question_index": "zero", "question_text": "q", "user_answer": "a"}`},
		{"malformed-json", `not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, isErr, _ := c.runTool(context.Background(), judgeTURaw(tc.raw), rag.ToolSet{Judge: j}, nil)
			if !isErr {
				t.Errorf("%s: expected isErr=true", tc.name)
			}
		})
	}
	if j.calls != 0 {
		t.Errorf("judge must not be called for invalid input, got %d calls", j.calls)
	}
}

func TestRunEval_JudgeError_HiddenFromUser(t *testing.T) {
	t.Parallel()
	const internal = "secret upstream 503 from haiku"
	j := &stubJudge{err: errors.New(internal)}
	c := newTestClient()

	text, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 0,
		"question_text":  "q",
		"user_answer":    "a thoughtful answer here",
	}), rag.ToolSet{Judge: j}, nil)
	if !isErr {
		t.Error("expected isErr=true when judge fails")
	}
	if strings.Contains(text, internal) {
		t.Errorf("internal error detail must not leak into result, got %q", text)
	}
}

// --- normalizeEvaluation clamps ---

func TestRunEval_ClampsScoreAndDerivesPassed(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	cases := []struct {
		in         int
		wantScore  int
		wantPassed bool
	}{
		{-50, 0, false},
		{0, 0, false},
		{59, 59, false},
		{60, 60, true},
		{85, 85, true},
		{150, 100, true},
	}
	for _, tc := range cases {
		j := &stubJudge{eval: &rag.InterviewEvaluation{Score: tc.in, Passed: !tc.wantPassed /* prove derived */}}
		text, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
			"question_index": 2,
			"question_text":  "q",
			"user_answer":    "a sufficiently long answer body",
		}), rag.ToolSet{Judge: j}, nil)
		if isErr {
			t.Fatalf("in=%d: unexpected error: %q", tc.in, text)
		}
		got := parseEval(t, text)
		if got.Score != tc.wantScore {
			t.Errorf("in=%d: score got %d, want %d", tc.in, got.Score, tc.wantScore)
		}
		if got.Passed != tc.wantPassed {
			t.Errorf("in=%d: passed got %v, want %v", tc.in, got.Passed, tc.wantPassed)
		}
	}
}

func TestRunEval_TrimsAndCapsConcepts(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	long := strings.Repeat("x", maxJudgeConceptChars+20)
	concepts := []string{"singleflight", "   ", "", "circuit breaker", long}
	for i := 0; i < maxJudgeConcepts; i++ {
		concepts = append(concepts, "extra")
	}
	j := &stubJudge{eval: &rag.InterviewEvaluation{Score: 80, ConceptsDemonstrated: concepts}}

	text, _, _ := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 0,
		"question_text":  "q",
		"user_answer":    "long enough answer",
	}), rag.ToolSet{Judge: j}, nil)
	got := parseEval(t, text)
	if len(got.ConceptsDemonstrated) > maxJudgeConcepts {
		t.Errorf("concepts: got %d, want <=%d", len(got.ConceptsDemonstrated), maxJudgeConcepts)
	}
	for _, c := range got.ConceptsDemonstrated {
		if c == "" || strings.TrimSpace(c) != c {
			t.Errorf("concept %q must be trimmed and non-empty", c)
		}
		if r := []rune(c); len(r) > maxJudgeConceptChars {
			t.Errorf("concept length: got %d runes, want <=%d", len(r), maxJudgeConceptChars)
		}
	}
}

// --- trace emission: PII regression guard ---

func TestRunEval_EmitsToolCallStep_NoPIIInTrace(t *testing.T) {
	t.Parallel()
	const piiAnswer = "I would deploy SECRET-PROJECT-VESPA and call my friend at acme-corp."
	const piiFeedback = "FEEDBACK-MARKER-OMEGA mentioning the candidate's specifics."
	j := &stubJudge{eval: &rag.InterviewEvaluation{
		Score:    77,
		Feedback: piiFeedback,
		Passed:   true,
	}}
	c := newTestClient()
	onTrace, steps := captureTrace()

	text, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 0,
		"question_text":  "Flash sale.",
		"user_answer":    piiAnswer,
	}), rag.ToolSet{Judge: j}, onTrace)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	if len(*steps) != 1 {
		t.Fatalf("trace steps: got %d, want 1", len(*steps))
	}
	s := (*steps)[0]
	if s.Kind != rag.TraceKindToolCall || s.ToolCall == nil {
		t.Fatalf("expected a tool_call step with detail, got %+v", s)
	}
	if s.ToolCall.Tool != toolEvaluateInterviewAnswer {
		t.Errorf("Tool: got %q, want %q", s.ToolCall.Tool, toolEvaluateInterviewAnswer)
	}
	if s.ToolCall.Target != "" {
		t.Errorf("Target must be empty, got %q", s.ToolCall.Target)
	}
	if s.ToolCall.Outcome != outcomeOK {
		t.Errorf("Outcome: got %q, want %q", s.ToolCall.Outcome, outcomeOK)
	}
	if s.Label != judgeTraceLabel {
		t.Errorf("Label: got %q, want curated %q", s.Label, judgeTraceLabel)
	}
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal step: %v", err)
	}
	// Neither the raw answer nor the judge's feedback may surface in the trace JSONB.
	if strings.Contains(string(blob), "SECRET-PROJECT-VESPA") {
		t.Errorf("user_answer leaked into trace step: %s", blob)
	}
	if strings.Contains(string(blob), "FEEDBACK-MARKER-OMEGA") {
		t.Errorf("judge feedback leaked into trace step: %s", blob)
	}
}

func TestRunEval_EmitsToolCallStep_Error(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	onTrace, steps := captureTrace()

	// Malformed JSON → exactly one tool_call step with error outcome.
	c.runTool(context.Background(), judgeTURaw(`not-json`), rag.ToolSet{Judge: &stubJudge{}}, onTrace)

	if len(*steps) != 1 {
		t.Fatalf("trace steps: got %d, want 1", len(*steps))
	}
	s := (*steps)[0]
	if s.ToolCall == nil || s.ToolCall.Outcome != outcomeError {
		t.Errorf("expected tool_call step with error outcome, got %+v", s)
	}
}

func TestRunEval_TruncatesLongAnswer(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	j := &stubJudge{eval: &rag.InterviewEvaluation{Score: 50}}
	long := strings.Repeat("a", maxAnswerChars+500)

	_, isErr, _ := c.runTool(context.Background(), judgeTU(map[string]any{
		"question_index": 0,
		"question_text":  "q",
		"user_answer":    long,
	}), rag.ToolSet{Judge: j}, nil)
	if isErr {
		t.Fatal("unexpected error")
	}
	if r := []rune(j.gotA); len(r) > maxAnswerChars {
		t.Errorf("judge received %d runes, want <=%d (wrapper must truncate)", len(r), maxAnswerChars)
	}
}

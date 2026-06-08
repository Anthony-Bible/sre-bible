package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// stubMatcher returns configured chunks per requirement and records every call.
type stubMatcher struct {
	mu     sync.Mutex
	byReq  map[string][]rag.RetrievedChunk
	defalt []rag.RetrievedChunk // returned when a requirement has no byReq entry
	err    error
	calls  []string
	gotK   int
}

func (m *stubMatcher) MatchRequirement(_ context.Context, req string, k int) ([]rag.RetrievedChunk, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	m.gotK = k
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if chunks, ok := m.byReq[req]; ok {
		return chunks, nil
	}
	return m.defalt, nil
}

func matchTU(reqs []string) anthropic.ToolUseBlock {
	raw, _ := json.Marshal(map[string]any{"requirements": reqs})
	return anthropic.ToolUseBlock{ID: "m1", Name: toolMatchJobDescription, Input: json.RawMessage(raw)}
}

func parseMatchResult(t *testing.T, text string) matchJobResult {
	t.Helper()
	var got matchJobResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("tool result is not valid matchJobResult JSON: %v\nraw: %q", err, text)
	}
	return got
}

// --- buildToolParams gating ---

func TestBuildToolParams_NoMatcherOmitsTool(t *testing.T) {
	t.Parallel()
	params := buildToolParams(rag.ToolSet{Lister: stubLister{}})
	for _, p := range params {
		if p.OfTool != nil && p.OfTool.Name == toolMatchJobDescription {
			t.Fatal("match_job_description should not be present when Matcher is nil")
		}
	}
}

func TestBuildToolParams_WithMatcherIncludesTool(t *testing.T) {
	t.Parallel()
	params := buildToolParams(rag.ToolSet{Matcher: &stubMatcher{}})
	var tool *anthropic.ToolParam
	for i := range params {
		if params[i].OfTool != nil && params[i].OfTool.Name == toolMatchJobDescription {
			tool = params[i].OfTool
		}
	}
	if tool == nil {
		t.Fatal("match_job_description should be present when Matcher is non-nil")
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != fieldRequirements {
		t.Errorf("required fields: got %v, want [%s]", tool.InputSchema.Required, fieldRequirements)
	}
}

// --- runMatchJobDescription ---

func TestRunMatch_EmptyRequirements_Error(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Matcher: &stubMatcher{}}

	cases := [][]string{nil, {}, {"", "   "}}
	for _, reqs := range cases {
		_, isErr, sources := c.runTool(context.Background(), matchTU(reqs), tools, nil)
		if !isErr {
			t.Errorf("requirements=%v: expected isErr=true", reqs)
		}
		if len(sources) != 0 {
			t.Errorf("requirements=%v: expected no sources, got %v", reqs, sources)
		}
	}
}

func TestRunMatch_MalformedJSON_Error(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Matcher: &stubMatcher{}}
	tu := anthropic.ToolUseBlock{ID: "m", Name: toolMatchJobDescription, Input: json.RawMessage(`not-json`)}

	_, isErr, _ := c.runTool(context.Background(), tu, tools, nil)
	if !isErr {
		t.Error("expected isErr=true for malformed JSON input")
	}
}

func TestRunMatch_TruncatesToMaxRequirements(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	m := &stubMatcher{}
	tools := rag.ToolSet{Matcher: m}

	reqs := make([]string, maxRequirements+5)
	for i := range reqs {
		reqs[i] = fmt.Sprintf("requirement %d", i)
	}

	text, isErr, _ := c.runTool(context.Background(), matchTU(reqs), tools, nil)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	if len(m.calls) != maxRequirements {
		t.Errorf("matcher called %d times, want %d (capped)", len(m.calls), maxRequirements)
	}
	got := parseMatchResult(t, text)
	if len(got.Requirements) != maxRequirements {
		t.Errorf("result has %d requirements, want %d", len(got.Requirements), maxRequirements)
	}
}

func TestRunMatch_EvidenceAndDedupedSources(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	m := &stubMatcher{
		byReq: map[string][]rag.RetrievedChunk{
			"Kubernetes": {
				{Content: "Ran K8s clusters", SourceName: "resume.pdf"},
				{Content: "More k8s", SourceName: "about.html"},
			},
			"Terraform": {
				{Content: "IaC with Terraform", SourceName: "resume.pdf"}, // dup source
			},
		},
	}
	tools := rag.ToolSet{Matcher: m}

	text, isErr, sources := c.runTool(context.Background(), matchTU([]string{"Kubernetes", "Terraform"}), tools, nil)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}

	// k defaults to the per-requirement evidence depth.
	if m.gotK != matchEvidenceK {
		t.Errorf("matcher received k=%d, want %d", m.gotK, matchEvidenceK)
	}

	// Sources are deduped across requirements, preserving first-seen order.
	wantSources := []string{"resume.pdf", "about.html"}
	if len(sources) != len(wantSources) {
		t.Fatalf("sources: got %v, want %v", sources, wantSources)
	}
	for i, w := range wantSources {
		if sources[i] != w {
			t.Errorf("sources[%d]: got %q, want %q", i, sources[i], w)
		}
	}

	got := parseMatchResult(t, text)
	byReq := map[string]matchRequirementResult{}
	for _, r := range got.Requirements {
		byReq[r.Requirement] = r
	}
	if n := len(byReq["Kubernetes"].Evidence); n != 2 {
		t.Errorf("Kubernetes evidence count: got %d, want 2", n)
	}
	if ev := byReq["Kubernetes"].Evidence; len(ev) > 0 && ev[0].Source != "resume.pdf" {
		t.Errorf("first Kubernetes evidence source: got %q, want resume.pdf", ev[0].Source)
	}
}

func TestRunMatch_NoChunks_EmptyEvidenceIsGap(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	// stubMatcher with no byReq entries and nil default → every requirement is a Gap.
	tools := rag.ToolSet{Matcher: &stubMatcher{}}

	text, isErr, sources := c.runTool(context.Background(), matchTU([]string{"COBOL"}), tools, nil)
	if isErr {
		t.Fatalf("a no-evidence requirement must not be an error: %q", text)
	}
	if len(sources) != 0 {
		t.Errorf("expected no sources for a Gap, got %v", sources)
	}
	got := parseMatchResult(t, text)
	if len(got.Requirements) != 1 {
		t.Fatalf("want 1 requirement, got %d", len(got.Requirements))
	}
	if len(got.Requirements[0].Evidence) != 0 {
		t.Errorf("Gap requirement must have empty evidence, got %v", got.Requirements[0].Evidence)
	}
}

func TestRunMatch_MatcherError_Hidden(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	const internal = "secret pg error 42"
	tools := rag.ToolSet{Matcher: &stubMatcher{err: errors.New(internal)}}

	text, isErr, _ := c.runTool(context.Background(), matchTU([]string{"anything"}), tools, nil)
	if !isErr {
		t.Error("expected isErr=true when retrieval fails")
	}
	if strings.Contains(text, internal) {
		t.Errorf("internal error detail must not leak into result, got %q", text)
	}
}

func TestRunMatch_ExcerptTruncated(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	long := strings.Repeat("x", maxEvidenceExcerpt+50)
	tools := rag.ToolSet{Matcher: &stubMatcher{
		byReq: map[string][]rag.RetrievedChunk{
			"req": {{Content: long, SourceName: "s"}},
		},
	}}

	text, _, _ := c.runTool(context.Background(), matchTU([]string{"req"}), tools, nil)
	got := parseMatchResult(t, text)
	ev := got.Requirements[0].Evidence
	if len(ev) != 1 {
		t.Fatalf("want 1 evidence item, got %d", len(ev))
	}
	if r := []rune(ev[0].Excerpt); len(r) != maxEvidenceExcerpt {
		t.Errorf("excerpt length: got %d runes, want %d", len(r), maxEvidenceExcerpt)
	}
}

// TestRunMatch_EmitsToolCallStep is a PII regression guard: the match_job_description
// trace step must carry the generic curated label and an empty target — never the
// Viewer's pasted requirement text.
func TestRunMatch_EmitsToolCallStep(t *testing.T) {
	t.Parallel()
	const piiReq = "Owns the ACME payroll migration SECRET-PROJECT-X"
	m := &stubMatcher{byReq: map[string][]rag.RetrievedChunk{
		piiReq: {{Content: "evidence", SourceName: "resume.pdf"}},
	}}
	c := newTestClient()
	onTrace, steps := captureTrace()

	text, isErr, _ := c.runTool(context.Background(), matchTU([]string{piiReq}), rag.ToolSet{Matcher: m}, onTrace)
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
	if s.ToolCall.Tool != toolMatchJobDescription {
		t.Errorf("Tool: got %q, want %q", s.ToolCall.Tool, toolMatchJobDescription)
	}
	if s.ToolCall.Target != "" {
		t.Errorf("Target must be empty for match_job_description, got %q", s.ToolCall.Target)
	}
	if s.ToolCall.Outcome != outcomeOK {
		t.Errorf("Outcome: got %q, want %q", s.ToolCall.Outcome, outcomeOK)
	}
	if s.Label != matchTraceLabel {
		t.Errorf("Label: got %q, want curated %q", s.Label, matchTraceLabel)
	}
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal step: %v", err)
	}
	if strings.Contains(string(blob), "SECRET-PROJECT-X") {
		t.Errorf("requirement text leaked into trace step: %s", blob)
	}
}

func TestRunMatch_EmitsToolCallStep_Error(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	onTrace, steps := captureTrace()

	// Empty requirements → error outcome, still exactly one tool_call step.
	c.runTool(context.Background(), matchTU(nil), rag.ToolSet{Matcher: &stubMatcher{}}, onTrace)

	if len(*steps) != 1 {
		t.Fatalf("trace steps: got %d, want 1", len(*steps))
	}
	if s := (*steps)[0]; s.ToolCall == nil || s.ToolCall.Outcome != outcomeError {
		t.Errorf("expected tool_call step with error outcome, got %+v", s)
	}
}

// TestRunMatch_ResultCarriesRenderInstructions asserts the rendering recipe rides in the
// tool result (the "tool response"), not just the static tool description.
func TestRunMatch_ResultCarriesRenderInstructions(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Matcher: &stubMatcher{
		byReq: map[string][]rag.RetrievedChunk{"req": {{Content: "e", SourceName: "s"}}},
	}}

	text, isErr, _ := c.runTool(context.Background(), matchTU([]string{"req"}), tools, nil)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	got := parseMatchResult(t, text)
	if got.Instructions != matchRenderInstructions {
		t.Errorf("result Instructions: got %q, want the render recipe", got.Instructions)
	}
}

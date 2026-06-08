package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Anthony-Bible/sre-bible/internal/email"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// --- stubs ---

type stubLister struct{}

func (stubLister) ListSources(_ context.Context) ([]rag.DocumentInfo, error) {
	return []rag.DocumentInfo{{Name: "doc.pdf", Type: "pdf"}}, nil
}

type stubFetcher struct{}

func (stubFetcher) GetFullText(_ context.Context, _ string) (string, bool, error) {
	return "full text", true, nil
}

type stubEmailer struct {
	ok     bool
	reason string
	err    error
}

func (s *stubEmailer) SendContactEmail(_ context.Context, _ email.ContactEmail) (bool, string, error) {
	return s.ok, s.reason, s.err
}

// --- buildToolParams ---

func TestBuildToolParams_NoEmailerOmitsTool(t *testing.T) {
	t.Parallel()
	tools := rag.ToolSet{Lister: stubLister{}, Fetcher: stubFetcher{}}
	params := buildToolParams(tools)
	for _, p := range params {
		if p.OfTool != nil && p.OfTool.Name == toolSendContactEmail {
			t.Fatal("send_contact_email should not be present when Emailer is nil")
		}
	}
}

func TestBuildToolParams_WithEmailerIncludesTool(t *testing.T) {
	t.Parallel()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: true}}
	params := buildToolParams(tools)
	var found bool
	for _, p := range params {
		if p.OfTool != nil && p.OfTool.Name == toolSendContactEmail {
			found = true
		}
	}
	if !found {
		t.Fatal("send_contact_email should be present when Emailer is non-nil")
	}
}

func TestBuildToolParams_EmailToolHasRequiredFields(t *testing.T) {
	t.Parallel()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: true}}
	params := buildToolParams(tools)

	var tool *anthropic.ToolParam
	for i := range params {
		if params[i].OfTool != nil && params[i].OfTool.Name == toolSendContactEmail {
			tool = params[i].OfTool
			break
		}
	}
	if tool == nil {
		t.Fatal("send_contact_email tool not found")
	}

	required := tool.InputSchema.Required
	want := []string{fieldSenderName, fieldSenderEmail, fieldMessage, fieldConfirmedDraft}
	if len(required) != len(want) {
		t.Fatalf("required fields: got %v, want %v", required, want)
	}
	for i, f := range want {
		if required[i] != f {
			t.Errorf("required[%d]: got %q, want %q", i, required[i], f)
		}
	}
}

// --- runTool: send_contact_email ---

func newTestClient() *Client {
	return &Client{
		model: "test",
		log:   slog.Default(),
	}
}

func makeTU(input any) anthropic.ToolUseBlock {
	raw, _ := json.Marshal(input)
	return anthropic.ToolUseBlock{ID: "id1", Name: toolSendContactEmail, Input: json.RawMessage(raw)}
}

func emailInput(confirmed bool) map[string]any {
	return map[string]any{
		"sender_name":     "Alice",
		"sender_email":    "alice@example.com",
		"message":         "Hello Anthony",
		"confirmed_draft": confirmed,
	}
}

func TestRunTool_EmailSuccess(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: true}}
	tu := makeTU(emailInput(true))

	text, isErr, sourceName := c.runTool(context.Background(), tu, tools, nil)

	if isErr {
		t.Errorf("expected isErr=false on success, got true; text=%q", text)
	}
	if sourceName != "" {
		t.Errorf("sourceName must be empty for email tool, got %q", sourceName)
	}
	if !strings.Contains(text, "sent") {
		t.Errorf("success text should mention 'sent', got %q", text)
	}
}

func TestRunTool_EmailUnconfirmedDraft_Rejected(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: true}}
	tu := makeTU(emailInput(false))

	_, isErr, _ := c.runTool(context.Background(), tu, tools, nil)

	if !isErr {
		t.Error("expected isErr=true when confirmed_draft=false")
	}
}

func TestRunTool_EmailRefusal(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: false, reason: "already sent"}}
	tu := makeTU(emailInput(true))

	text, isErr, sourceName := c.runTool(context.Background(), tu, tools, nil)

	if !isErr {
		t.Errorf("expected isErr=true for refusal, got false")
	}
	if sourceName != "" {
		t.Errorf("sourceName must be empty, got %q", sourceName)
	}
	if text != "already sent" {
		t.Errorf("refusal reason should be relayed verbatim, got %q", text)
	}
}

func TestRunTool_EmailInternalError_MessageHidesDetails(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	const internalMsg = "secret db error xyz"
	tools := rag.ToolSet{Emailer: &stubEmailer{err: &testError{internalMsg}}}
	tu := makeTU(emailInput(true))

	text, isErr, _ := c.runTool(context.Background(), tu, tools, nil)

	if !isErr {
		t.Errorf("expected isErr=true for internal error, got false")
	}
	if strings.Contains(text, internalMsg) {
		t.Errorf("internal error detail must not leak into result text, got %q", text)
	}
}

func TestRunTool_EmailMalformedJSON(t *testing.T) {
	t.Parallel()
	c := newTestClient()
	tools := rag.ToolSet{Emailer: &stubEmailer{ok: true}}
	tu := anthropic.ToolUseBlock{ID: "id2", Name: toolSendContactEmail, Input: json.RawMessage(`not-json`)}

	_, isErr, _ := c.runTool(context.Background(), tu, tools, nil)

	if !isErr {
		t.Error("expected isErr=true for malformed JSON input")
	}
}

// --- runTool: list_documents formatting ---

type configuredLister struct {
	docs []rag.DocumentInfo
}

func (l configuredLister) ListSources(_ context.Context) ([]rag.DocumentInfo, error) {
	return l.docs, nil
}

func TestRunTool_ListDocuments_Formatting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		docs     []rag.DocumentInfo
		wantLine string
	}{
		{
			name:     "with description",
			docs:     []rag.DocumentInfo{{Name: "resume.pdf", Type: "pdf", Description: "Anthony's SRE resume."}},
			wantLine: "resume.pdf (pdf): Anthony's SRE resume.",
		},
		{
			name:     "without description (legacy NULL)",
			docs:     []rag.DocumentInfo{{Name: "resume.pdf", Type: "pdf"}},
			wantLine: "resume.pdf (pdf)",
		},
		{
			name: "mixed: one with, one without",
			docs: []rag.DocumentInfo{
				{Name: "a.pdf", Type: "pdf", Description: "Document A."},
				{Name: "b.url", Type: "url"},
			},
			wantLine: "a.pdf (pdf): Document A.\nb.url (url)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient()
			tools := rag.ToolSet{Lister: configuredLister{docs: tc.docs}}
			tu := anthropic.ToolUseBlock{ID: "id", Name: toolListDocuments, Input: json.RawMessage(`{}`)}

			text, isErr, sourceName := c.runTool(context.Background(), tu, tools, nil)

			if isErr {
				t.Errorf("expected isErr=false, got true; text=%q", text)
			}
			if sourceName != "" {
				t.Errorf("sourceName must be empty for list_documents, got %q", sourceName)
			}
			got := strings.TrimRight(text, "\n")
			if got != tc.wantLine {
				t.Errorf("output:\n  got  %q\n  want %q", got, tc.wantLine)
			}
		})
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// --- tool_call trace steps ---
//
// These exercise the runTool seam (StreamAnswer itself is unmockable — it drives the
// live Anthropic SDK). The terminal "answer" step is NOT unit-tested here: it is emitted
// only inside StreamAnswer's loop, and is covered indirectly via the pipeline stub
// (internal/rag) and the SSE handler tests (internal/server).

// captureTrace returns an onTrace callback plus a pointer to the slice it appends to.
func captureTrace() (func(rag.TraceStep) error, *[]rag.TraceStep) {
	var steps []rag.TraceStep
	return func(s rag.TraceStep) error {
		steps = append(steps, s)
		return nil
	}, &steps
}

// fetcherWith lets a test control GetFullText's return values.
type fetcherWith struct {
	text  string
	found bool
	err   error
}

func (f fetcherWith) GetFullText(_ context.Context, _ string) (string, bool, error) {
	return f.text, f.found, f.err
}

// errLister returns an error from ListSources.
type errLister struct{}

func (errLister) ListSources(_ context.Context) ([]rag.DocumentInfo, error) {
	return nil, &testError{"db down"}
}

func TestRunTool_EmitsToolCallStep_ListDocuments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		tools       rag.ToolSet
		wantOutcome string
	}{
		{"ok", rag.ToolSet{Lister: stubLister{}}, outcomeOK},
		{"error", rag.ToolSet{Lister: errLister{}}, outcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient()
			onTrace, steps := captureTrace()
			tu := anthropic.ToolUseBlock{ID: "id", Name: toolListDocuments, Input: json.RawMessage(`{}`)}

			c.runTool(context.Background(), tu, tc.tools, onTrace)

			if len(*steps) != 1 {
				t.Fatalf("trace steps: got %d, want 1", len(*steps))
			}
			s := (*steps)[0]
			if s.Kind != rag.TraceKindToolCall || s.ToolCall == nil {
				t.Fatalf("expected a tool_call step with detail, got %+v", s)
			}
			if s.ToolCall.Tool != toolListDocuments {
				t.Errorf("Tool: got %q, want %q", s.ToolCall.Tool, toolListDocuments)
			}
			if s.ToolCall.Target != "" {
				t.Errorf("Target must be empty for list_documents, got %q", s.ToolCall.Target)
			}
			if s.ToolCall.Outcome != tc.wantOutcome {
				t.Errorf("Outcome: got %q, want %q", s.ToolCall.Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestRunTool_EmitsToolCallStep_FetchFullDocument(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		fetcher     fetcherWith
		input       string
		wantOutcome string
		wantTarget  string
	}{
		{"ok", fetcherWith{text: "full", found: true}, `{"source_name":"resume.pdf"}`, outcomeOK, "resume.pdf"},
		{"not_found", fetcherWith{found: false}, `{"source_name":"ghost.pdf"}`, outcomeNotFound, "ghost.pdf"},
		{"error", fetcherWith{err: &testError{"boom"}}, `{"source_name":"resume.pdf"}`, outcomeError, "resume.pdf"},
		{"malformed", fetcherWith{}, `not-json`, outcomeError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient()
			onTrace, steps := captureTrace()
			tu := anthropic.ToolUseBlock{ID: "id", Name: toolFetchFullDocument, Input: json.RawMessage(tc.input)}

			c.runTool(context.Background(), tu, rag.ToolSet{Fetcher: tc.fetcher}, onTrace)

			if len(*steps) != 1 {
				t.Fatalf("trace steps: got %d, want 1", len(*steps))
			}
			s := (*steps)[0]
			if s.Kind != rag.TraceKindToolCall || s.ToolCall == nil {
				t.Fatalf("expected a tool_call step with detail, got %+v", s)
			}
			if s.ToolCall.Tool != toolFetchFullDocument {
				t.Errorf("Tool: got %q, want %q", s.ToolCall.Tool, toolFetchFullDocument)
			}
			if s.ToolCall.Target != tc.wantTarget {
				t.Errorf("Target: got %q, want %q", s.ToolCall.Target, tc.wantTarget)
			}
			if s.ToolCall.Outcome != tc.wantOutcome {
				t.Errorf("Outcome: got %q, want %q", s.ToolCall.Outcome, tc.wantOutcome)
			}
		})
	}
}

// TestToolCallStep_EmailHasNoPII is a PII regression guard: the send_contact_email
// trace step must NEVER carry the Viewer's name, email, message body, or refusal reason.
// Target must always be empty and the Label must be the generic curated string.
func TestToolCallStep_EmailHasNoPII(t *testing.T) {
	t.Parallel()

	const (
		piiName  = "Alice Privacy"
		piiEmail = "alice.private@example.com"
		piiBody  = "TOP SECRET hire me immediately"
	)
	emailWithPII := func(confirmed bool) map[string]any {
		return map[string]any{
			"sender_name":     piiName,
			"sender_email":    piiEmail,
			"message":         piiBody,
			"confirmed_draft": confirmed,
		}
	}

	cases := []struct {
		name        string
		emailer     *stubEmailer
		confirmed   bool
		wantOutcome string
	}{
		{"success", &stubEmailer{ok: true}, true, outcomeOK},
		{"unconfirmed", &stubEmailer{ok: true}, false, outcomeRefused},
		{"refused", &stubEmailer{ok: false, reason: "rate limited: 1/session"}, true, outcomeRefused},
		{"internal_error", &stubEmailer{err: &testError{"smtp creds leaked here"}}, true, outcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient()
			onTrace, steps := captureTrace()
			tu := makeTU(emailWithPII(tc.confirmed))

			c.runTool(context.Background(), tu, rag.ToolSet{Emailer: tc.emailer}, onTrace)

			if len(*steps) != 1 {
				t.Fatalf("trace steps: got %d, want 1", len(*steps))
			}
			s := (*steps)[0]
			if s.Kind != rag.TraceKindToolCall || s.ToolCall == nil {
				t.Fatalf("expected a tool_call step with detail, got %+v", s)
			}
			if s.ToolCall.Tool != toolSendContactEmail {
				t.Errorf("Tool: got %q, want %q", s.ToolCall.Tool, toolSendContactEmail)
			}
			if s.ToolCall.Outcome != tc.wantOutcome {
				t.Errorf("Outcome: got %q, want %q", s.ToolCall.Outcome, tc.wantOutcome)
			}
			// Curated label, empty target.
			if s.Label != emailTraceLabel {
				t.Errorf("Label: got %q, want curated %q", s.Label, emailTraceLabel)
			}
			if s.ToolCall.Target != "" {
				t.Errorf("Target must be empty for send_contact_email, got %q", s.ToolCall.Target)
			}
			// No PII anywhere in the serialised step.
			blob, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("marshal step: %v", err)
			}
			for _, secret := range []string{piiName, piiEmail, piiBody, tc.emailer.reason} {
				if secret != "" && strings.Contains(string(blob), secret) {
					t.Errorf("PII leak: trace step contains %q\n  step JSON: %s", secret, blob)
				}
			}
		})
	}
}

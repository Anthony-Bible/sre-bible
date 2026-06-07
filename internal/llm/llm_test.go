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

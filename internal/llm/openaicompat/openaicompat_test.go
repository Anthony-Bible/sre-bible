package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// capturedRequest is the subset of the OpenAI chat-completions request body the tests
// assert on.
type capturedRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

// completionWith builds an OpenAI chat-completion response whose single choice carries
// content (the model's raw JSON string output).
func completionWith(content string) string {
	body, _ := json.Marshal(map[string]any{
		"id":      "cmpl-1",
		"object":  "chat.completion",
		"created": 0,
		"model":   "test-model",
		"choices": []map[string]any{
			{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
		},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	return string(body)
}

// serve spins up an httptest server returning the given response body and records the
// last request's parsed body and Authorization header. apiKey is passed to New.
func serve(t *testing.T, apiKey, responseBody string, status int) (*Suggester, *capturedRequest, *string) {
	t.Helper()

	var req capturedRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(srv.Close)

	return New(srv.URL, apiKey, "test-model", nil, nil), &req, &auth
}

func TestSuggestFollowUps_ObjectShape_ParsedAndCapped(t *testing.T) {
	t.Parallel()
	s, req, _ := serve(t, "secret-key", completionWith(`{"questions":["A","  B  ","C"]}`), 0)

	got, err := s.SuggestFollowUps(context.Background(), "SCOPE-LOCKED", []rag.Message{
		{Role: rag.RoleUser, Content: "first"},
		{Role: rag.RoleAssistant, Content: "answer"},
	}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"A", "B"}
	if len(got) != len(want) {
		t.Fatalf("questions: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("questions[%d]: got %q, want %q", i, got[i], want[i])
		}
	}

	// Contract: json_object response format, model, system + mapped roles.
	if req.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format.type: got %q, want %q", req.ResponseFormat.Type, "json_object")
	}
	if req.Model != "test-model" {
		t.Errorf("model: got %q, want %q", req.Model, "test-model")
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages: got %d, want 3 (system + 2 history)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "SCOPE-LOCKED" {
		t.Errorf("first message must be the system prompt, got %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[2].Role != "assistant" {
		t.Errorf("history roles not mapped: got %q,%q", req.Messages[1].Role, req.Messages[2].Role)
	}
}

func TestSuggestFollowUps_BareArrayFallback(t *testing.T) {
	t.Parallel()
	// Server honours the prompt but ignores response_format, emitting a bare array.
	s, _, _ := serve(t, "k", completionWith(`["X","Y"]`), 0)

	got, err := s.SuggestFollowUps(context.Background(), "p", []rag.Message{{Role: rag.RoleUser, Content: "q"}}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"X", "Y"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("bare-array fallback: got %v, want %v", got, want)
	}
}

func TestSuggestFollowUps_TruncatedArray_Errors(t *testing.T) {
	t.Parallel()
	// A verbose model ignored "at most 2", over-generated, and ran past MaxTokens —
	// truncating the JSON mid-string (finish_reason="length"). Truncated JSON is
	// unparseable and surfaces as an error: the feature degrades to no cards, and the
	// error keeps the underlying misconfiguration visible (logged, metered) rather than
	// silently recovering partial output. Fix the root cause by disabling reasoning.
	truncated := `{"questions": ["First question?", "Second question?", "Third question?", "What`
	s, _, _ := serve(t, "k", completionWith(truncated), 0)

	got, err := s.SuggestFollowUps(context.Background(), "p",
		[]rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2)
	if err == nil {
		t.Fatalf("truncated array must error (degrades to no cards), got %v", got)
	}
}

func TestSuggestFollowUps_UnparseableContent_Errors(t *testing.T) {
	t.Parallel()
	s, _, _ := serve(t, "k", completionWith(`this is not json`), 0)

	got, err := s.SuggestFollowUps(context.Background(), "p", []rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2)
	if err == nil {
		t.Fatalf("expected error for unparseable content, got %v", got)
	}
}

func TestSuggestFollowUps_EmptyContent_NoCardsNoError(t *testing.T) {
	t.Parallel()
	// A thinking model that exhausts MaxTokens on reasoning returns empty content with
	// finish_reason "length". That is a benign "no cards" outcome, not a parse error.
	body, _ := json.Marshal(map[string]any{
		"id": "cmpl-1", "object": "chat.completion", "model": "test-model",
		"choices": []map[string]any{
			{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": "length"},
		},
	})
	s, _, _ := serve(t, "k", string(body), 0)

	got, err := s.SuggestFollowUps(context.Background(), "p",
		[]rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2)
	if err != nil {
		t.Fatalf("empty content must degrade to no-cards, not error: %v", err)
	}
	if got != nil {
		t.Errorf("questions: got %v, want nil for an empty completion", got)
	}
}

// TestNew_ExtraBodyInjected proves the provider-neutral escape hatch: a key/value passed
// as extraBody (here GLM's reasoning-disable toggle) is merged into the outbound request
// body, which is how a thinking model is told not to spend the token budget on reasoning.
func TestNew_ExtraBodyInjected(t *testing.T) {
	t.Parallel()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completionWith(`{"questions":["A"]}`))
	}))
	t.Cleanup(srv.Close)

	s := New(srv.URL, "k", "test-model",
		map[string]any{"thinking": map[string]any{"type": "disabled"}}, nil)
	if _, err := s.SuggestFollowUps(context.Background(), "p",
		[]rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body struct {
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body.Thinking.Type != "disabled" {
		t.Errorf("extraBody not injected: thinking.type = %q, want %q", body.Thinking.Type, "disabled")
	}
}

func TestSuggestFollowUps_Non2xx_Errors(t *testing.T) {
	t.Parallel()
	// 400 is a client error the SDK does not retry, keeping the test fast.
	s, _, _ := serve(t, "k", `{"error":{"message":"bad request"}}`, http.StatusBadRequest)

	got, err := s.SuggestFollowUps(context.Background(), "p", []rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2)
	if err == nil {
		t.Fatalf("expected error for non-2xx status, got %v", got)
	}
}

func TestSuggestFollowUps_AuthPresentWhenKeySet(t *testing.T) {
	t.Parallel()
	s, _, auth := serve(t, "secret-key", completionWith(`{"questions":["A"]}`), 0)
	if _, err := s.SuggestFollowUps(context.Background(), "p",
		[]rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *auth != "Bearer secret-key" {
		t.Errorf("Authorization: got %q, want %q", *auth, "Bearer secret-key")
	}
}

// Not parallel: uses t.Setenv to neutralise any ambient OPENAI_API_KEY so the
// empty-key case is deterministic (the SDK would otherwise inherit an env key).
func TestSuggestFollowUps_AuthOmittedWhenKeyEmpty(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	s, _, auth := serve(t, "", completionWith(`{"questions":["A"]}`), 0)
	if _, err := s.SuggestFollowUps(context.Background(), "p",
		[]rag.Message{{Role: rag.RoleUser, Content: "q"}}, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No real token must be sent (guards the WithAPIKey("") -> "Bearer " gotcha).
	if token := strings.TrimSpace(strings.TrimPrefix(*auth, "Bearer")); token != "" {
		t.Errorf("Authorization must carry no token when apiKey empty, got %q", *auth)
	}
}

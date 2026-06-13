package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// newSuggestClient wires a *Client whose inner Anthropic SDK points at a local
// httptest server, so SuggestFollowUps (a non-streaming Messages.New call) can be
// exercised end-to-end without live network. handler serves the canned response.
func newSuggestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	inner := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	return &Client{inner: &inner, model: "test-model", log: slog.Default()}
}

// toolUseResponse builds a minimal Anthropic Messages response whose single content
// block is a forced suggest_questions tool_use carrying the given questions.
func toolUseResponse(questions []string) string {
	input, _ := json.Marshal(map[string]any{"questions": questions})
	body, _ := json.Marshal(map[string]any{
		"id":    "msg_1",
		"type":  "message",
		"role":  "assistant",
		"model": "test-model",
		"content": []map[string]any{
			{"type": "tool_use", "id": "tu_1", "name": toolSuggestQuestions, "input": json.RawMessage(input)},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	return string(body)
}

func TestSuggestFollowUps_ForcedToolPath(t *testing.T) {
	t.Parallel()

	var captured struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}

	c := newSuggestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &captured); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// Return three questions; the cap (2) must trim the third.
		_, _ = io.WriteString(w, toolUseResponse([]string{"Q1", "  Q2  ", "Q3"}))
	})

	history := []rag.Message{
		{Role: rag.RoleUser, Content: "first"},
		{Role: rag.RoleAssistant, Content: "answer"},
		{Role: rag.RoleUser, Content: "catalog instruction"},
	}

	got, err := c.SuggestFollowUps(context.Background(), "SCOPE-LOCKED PROMPT", history, 2)
	if err != nil {
		t.Fatalf("SuggestFollowUps: unexpected error: %v", err)
	}

	// Contract: parsed, trimmed, and capped to maxQuestions.
	want := []string{"Q1", "Q2"}
	if len(got) != len(want) {
		t.Fatalf("questions: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("questions[%d]: got %q, want %q", i, got[i], want[i])
		}
	}

	// Contract: forced tool, scope-locked system prompt, mapped roles.
	if captured.Model != "test-model" {
		t.Errorf("model: got %q, want %q", captured.Model, "test-model")
	}
	if captured.MaxTokens != rag.MaxFollowUpTokens {
		t.Errorf("max_tokens: got %d, want %d", captured.MaxTokens, rag.MaxFollowUpTokens)
	}
	if len(captured.System) != 1 || captured.System[0].Text != "SCOPE-LOCKED PROMPT" {
		t.Errorf("system prompt not forwarded verbatim: got %+v", captured.System)
	}
	if captured.ToolChoice.Type != "tool" || captured.ToolChoice.Name != toolSuggestQuestions {
		t.Errorf("tool_choice: got %+v, want forced %q", captured.ToolChoice, toolSuggestQuestions)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Name != toolSuggestQuestions {
		t.Errorf("tools: got %+v, want single %q", captured.Tools, toolSuggestQuestions)
	}
	if len(captured.Messages) != 3 {
		t.Fatalf("messages: got %d, want 3 (full history forwarded)", len(captured.Messages))
	}
	if captured.Messages[0].Role != "user" || captured.Messages[1].Role != "assistant" {
		t.Errorf("message roles not mapped: got %q,%q", captured.Messages[0].Role, captured.Messages[1].Role)
	}
}

func TestSuggestFollowUps_NoToolUseBlock_Errors(t *testing.T) {
	t.Parallel()

	c := newSuggestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A text-only response (model declined the forced tool) — must surface as error.
		body, _ := json.Marshal(map[string]any{
			"id":          "msg_2",
			"type":        "message",
			"role":        "assistant",
			"model":       "test-model",
			"content":     []map[string]any{{"type": "text", "text": "no tool here"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
		_, _ = w.Write(body)
	})

	got, err := c.SuggestFollowUps(context.Background(),
		"prompt", []rag.Message{{Role: rag.RoleUser, Content: "hi"}}, 2)
	if err == nil {
		t.Fatalf("expected error when no tool_use block present, got questions=%v", got)
	}
}

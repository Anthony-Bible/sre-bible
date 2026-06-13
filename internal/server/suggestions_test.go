package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// ---------------------------------------------------------------------------
// Stub: a pipeline that satisfies BOTH Answerer and Suggester
// ---------------------------------------------------------------------------

// stubSuggestPipeline embeds stubPipeline (Answerer) and adds the Suggester
// method so NewServer's type assertion enables the POST /suggestions endpoint.
// A plain *stubPipeline does NOT implement Suggester, which is exactly how the
// "feature disabled" case is exercised.
type stubSuggestPipeline struct {
	stubPipeline

	questions    []string
	suggestErr   error
	suggestCalls int
}

func (p *stubSuggestPipeline) SuggestFollowUps(_ context.Context, _ []rag.Message) ([]string, error) {
	p.suggestCalls++
	return p.questions, p.suggestErr
}

// decodeSuggestions decodes a /suggestions response body into its questions slice.
func decodeSuggestions(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var dto suggestionsDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatalf("decode suggestions body: %v", err)
	}
	return dto.Questions
}

// suggestRequest builds a POST /suggestions request carrying a valid session header.
func suggestRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/suggestions", nil)
	req.Header.Set(sessionHeader, validSessionFixture)
	return req
}

// oneTurn is a minimal non-empty history so the handler reaches the suggester.
func oneTurn() []StoredMessage {
	return []StoredMessage{
		{Message: rag.Message{Role: rag.RoleUser, Content: "what did Anthony do at X?"}},
		{Message: rag.Message{Role: rag.RoleAssistant, Content: "He ran the platform."}},
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_UnverifiedSession_Forbidden
// ---------------------------------------------------------------------------

// TestHandleSuggestions_UnverifiedSession_Forbidden verifies the abuse gate: when
// Turnstile is configured and the session has not yet passed it, POST /suggestions
// returns 403 and never calls the LLM suggester.
func TestHandleSuggestions_UnverifiedSession_Forbidden(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{isVerified: false, messages: oneTurn()}
	pipeline := &stubSuggestPipeline{questions: []string{"should not surface"}}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (unverified session)", rr.Code, http.StatusForbidden)
	}
	if pipeline.suggestCalls != 0 {
		t.Errorf("SuggestFollowUps called %d time(s), want 0 for an unverified session", pipeline.suggestCalls)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_VerifiedCheckFails
// ---------------------------------------------------------------------------

// TestHandleSuggestions_VerifiedCheckFails verifies that a DB error while checking
// session verification surfaces as 500 (an infrastructure failure, distinct from the
// silent no-cards degradation used for suggestion-generation failures).
func TestHandleSuggestions_VerifiedCheckFails(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{verifyErr: errors.New("db is on fire")}
	pipeline := &stubSuggestPipeline{questions: []string{"x"}}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (verification check failed)", rr.Code, http.StatusInternalServerError)
	}
	if pipeline.suggestCalls != 0 {
		t.Errorf("SuggestFollowUps called %d time(s), want 0 when the gate errors", pipeline.suggestCalls)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_FeatureDisabled_EmptyList
// ---------------------------------------------------------------------------

// TestHandleSuggestions_FeatureDisabled_EmptyList verifies that when the pipeline
// does not implement Suggester (feature off), POST /suggestions degrades to a 200
// with an empty, non-null questions array. Uses a verified session to prove the gate
// is passed and the empty list is the feature-off path, not the abuse path.
func TestHandleSuggestions_FeatureDisabled_EmptyList(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{isVerified: true, messages: oneTurn()}
	// stubPipeline implements Answerer only — NewServer leaves s.suggester nil.
	srv := newTestServerWithTurnstile(t, &stubPipeline{}, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (feature disabled)", rr.Code, http.StatusOK)
	}
	got := decodeSuggestions(t, rr)
	if got == nil {
		t.Error("questions must be a non-null empty array, got null")
	}
	if len(got) != 0 {
		t.Errorf("questions = %v, want empty when feature disabled", got)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_HappyPath
// ---------------------------------------------------------------------------

// TestHandleSuggestions_HappyPath verifies that a verified session with history
// returns the suggester's questions as JSON.
func TestHandleSuggestions_HappyPath(t *testing.T) {
	t.Parallel()

	want := []string{"What was Anthony's biggest reliability win?", "How did he reduce on-call load?"}
	sessions := &stubSessions{isVerified: true, messages: oneTurn()}
	pipeline := &stubSuggestPipeline{questions: want}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (happy path)", rr.Code, http.StatusOK)
	}
	if pipeline.suggestCalls != 1 {
		t.Errorf("SuggestFollowUps called %d time(s), want 1", pipeline.suggestCalls)
	}
	got := decodeSuggestions(t, rr)
	if len(got) != len(want) {
		t.Fatalf("questions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("questions[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_EmptyHistory_EmptyList
// ---------------------------------------------------------------------------

// TestHandleSuggestions_EmptyHistory_EmptyList verifies that with no stored messages
// the handler returns an empty list without invoking the suggester (nothing to ground
// suggestions on yet).
func TestHandleSuggestions_EmptyHistory_EmptyList(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{isVerified: true} // messages defaults to nil/empty
	pipeline := &stubSuggestPipeline{questions: []string{"should not surface"}}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if pipeline.suggestCalls != 0 {
		t.Errorf("SuggestFollowUps called %d time(s), want 0 for empty history", pipeline.suggestCalls)
	}
	if got := decodeSuggestions(t, rr); len(got) != 0 {
		t.Errorf("questions = %v, want empty for empty history", got)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_SuggesterError_EmptyList
// ---------------------------------------------------------------------------

// TestHandleSuggestions_SuggesterError_EmptyList verifies that a suggester failure
// degrades silently: HTTP 200 with an empty list. A missing suggestion must never
// surface as an error to the visitor.
func TestHandleSuggestions_SuggesterError_EmptyList(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{isVerified: true, messages: oneTurn()}
	pipeline := &stubSuggestPipeline{suggestErr: errors.New("llm unavailable")}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (suggester error degrades silently)", rr.Code, http.StatusOK)
	}
	got := decodeSuggestions(t, rr)
	if got == nil {
		t.Error("questions must be a non-null empty array, got null")
	}
	if len(got) != 0 {
		t.Errorf("questions = %v, want empty when suggester errors", got)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_BadSessionID
// ---------------------------------------------------------------------------

// TestHandleSuggestions_BadSessionID verifies a malformed X-Session-ID is rejected
// with 400 before any gate check or suggester call.
func TestHandleSuggestions_BadSessionID(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{isVerified: true, messages: oneTurn()}
	pipeline := &stubSuggestPipeline{questions: []string{"nope"}}
	srv := newTestServerWithTurnstile(t, pipeline, sessions, &stubTurnstile{})

	req := httptest.NewRequest(http.MethodPost, "/suggestions", nil)
	req.Header.Set(sessionHeader, "not-a-uuid")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (bad session id)", rr.Code, http.StatusBadRequest)
	}
	if pipeline.suggestCalls != 0 {
		t.Errorf("SuggestFollowUps called %d time(s), want 0 for a bad session id", pipeline.suggestCalls)
	}
}

// ---------------------------------------------------------------------------
// TestHandleSuggestions_NoTurnstile_SkipsGate
// ---------------------------------------------------------------------------

// TestHandleSuggestions_NoTurnstile_SkipsGate verifies that when Turnstile is not
// configured (local dev / tests) the verified gate is skipped entirely: an
// unverified session still gets its suggestions.
func TestHandleSuggestions_NoTurnstile_SkipsGate(t *testing.T) {
	t.Parallel()

	want := []string{"What technologies does Anthony specialise in?"}
	sessions := &stubSessions{isVerified: false, messages: oneTurn()}
	pipeline := &stubSuggestPipeline{questions: want}
	srv := newTestServer(t, pipeline, sessions) // nil turnstile → gate skipped

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, suggestRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (no turnstile)", rr.Code, http.StatusOK)
	}
	if pipeline.suggestCalls != 1 {
		t.Errorf("SuggestFollowUps called %d time(s), want 1", pipeline.suggestCalls)
	}
	if got := decodeSuggestions(t, rr); len(got) != 1 || got[0] != want[0] {
		t.Errorf("questions = %v, want %v", got, want)
	}
}

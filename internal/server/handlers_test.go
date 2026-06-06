package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// ---------------------------------------------------------------------------
// Stub types
// ---------------------------------------------------------------------------

// stubSessions implements SessionRepository with controllable behavior.
type stubSessions struct {
	createErr error
	listErr   error
	appendErr error
	messages  []StoredMessage
	appended  []appendedCall
}

type appendedCall struct {
	sessionID string
	msg       rag.Message
	citations []string
}

func (s *stubSessions) CreateSession(_ context.Context, _ string) error {
	return s.createErr
}

func (s *stubSessions) ListMessages(_ context.Context, _ string) ([]StoredMessage, error) {
	return s.messages, s.listErr
}

func (s *stubSessions) AppendMessage(_ context.Context, sid string, msg rag.Message, cit []string) error {
	s.appended = append(s.appended, appendedCall{sid, msg, cit})
	return s.appendErr
}

// stubPipeline implements Answerer with controllable tokens and citations.
type stubPipeline struct {
	tokens    []string
	citations []string
	err       error
}

func (p *stubPipeline) Answer(_ context.Context, _ []rag.Message, _ string, onToken func(string) error) ([]string, error) {
	if p.err != nil {
		return nil, p.err
	}
	for _, t := range p.tokens {
		if err := onToken(t); err != nil {
			return nil, err
		}
	}
	return p.citations, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestServer builds a *Server under test using NewServer so that the full
// route registration path (including mux wiring) is exercised.
func newTestServer(t *testing.T, pipeline Answerer, sessions SessionRepository) *Server {
	t.Helper()
	srv, err := NewServer(pipeline, sessions, nil, nil)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}
	return srv
}

// nonFlushingWriter wraps httptest.ResponseRecorder but intentionally does
// NOT expose an http.Flusher interface. httptest.ResponseRecorder gained
// Flush() in Go 1.22, so to exercise the "flusher unavailable" code path we
// must wrap it behind a plain http.ResponseWriter interface.
type nonFlushingWriter struct {
	rr *httptest.ResponseRecorder
}

func (nf *nonFlushingWriter) Header() http.Header         { return nf.rr.Header() }
func (nf *nonFlushingWriter) Write(b []byte) (int, error) { return nf.rr.Write(b) }
func (nf *nonFlushingWriter) WriteHeader(code int)        { nf.rr.WriteHeader(code) }

// Code returns the response status code captured by the inner recorder.
func (nf *nonFlushingWriter) Code() int { return nf.rr.Code }

// ---------------------------------------------------------------------------
// TestHandleIndex_NewVisitor
// ---------------------------------------------------------------------------

// TestHandleIndex_NewVisitor verifies that a GET / request with no session
// cookie receives a 200 response and has a Set-Cookie header written so the
// browser can persist the new session ID.
func TestHandleIndex_NewVisitor(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if rr.Header().Get("Set-Cookie") == "" {
		t.Error("expected a Set-Cookie header for a new visitor, got none")
	}
}

// ---------------------------------------------------------------------------
// TestHandleIndex_ReturningVisitor
// ---------------------------------------------------------------------------

// TestHandleIndex_ReturningVisitor verifies that a GET / request that already
// carries a valid session_id cookie is NOT issued a new Set-Cookie header —
// the server should leave the existing cookie untouched.
func TestHandleIndex_ReturningVisitor(t *testing.T) {
	t.Parallel()

	const existingSession = "aabbccdd-0000-4000-8000-000000000001"

	sessions := &stubSessions{}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: existingSession})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if rr.Header().Get("Set-Cookie") != "" {
		t.Errorf("expected no Set-Cookie header for a returning visitor, got: %q", rr.Header().Get("Set-Cookie"))
	}
}

// ---------------------------------------------------------------------------
// TestHandleIndex_ListMessagesFails
// ---------------------------------------------------------------------------

// TestHandleIndex_ListMessagesFails verifies that when the session repository
// returns an error from ListMessages the handler responds with 500 so the
// client gets an unambiguous signal that something went wrong rather than an
// empty or partial page.
func TestHandleIndex_ListMessagesFails(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{
		listErr: errors.New("db is on fire"),
	}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_EmptyQuestion
// ---------------------------------------------------------------------------

// TestHandleChat_EmptyQuestion verifies that a POST /chat with a blank
// question field is rejected immediately with 400 — no pipeline call should
// occur and no session data should be written.
func TestHandleChat_EmptyQuestion(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	pipeline := &stubPipeline{}
	srv := newTestServer(t, pipeline, sessions)

	form := url.Values{}
	form.Set("question", "   ") // whitespace only — TrimSpace makes it blank
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Give it an existing session cookie so CreateSession/ListMessages are not
	// the reason we get a 400 — the empty question guard must fire first.
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "aabbccdd-0000-4000-8000-000000000002"})

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}

	if len(sessions.appended) != 0 {
		t.Errorf("AppendMessage was called %d time(s), want 0 calls for an empty question", len(sessions.appended))
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_NoFlusher
// ---------------------------------------------------------------------------

// TestHandleChat_NoFlusher verifies that when the underlying ResponseWriter
// does not implement http.Flusher the handler responds with 500.  SSE
// requires flushing; a non-flushing writer would produce a broken stream, so
// the server must refuse the request rather than silently produce garbage.
//
// Note: httptest.ResponseRecorder gained http.Flusher in Go 1.22, so we use
// nonFlushingWriter — a thin wrapper that hides the Flush method — to
// exercise this code path.
func TestHandleChat_NoFlusher(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	srv := newTestServer(t, &stubPipeline{tokens: []string{"hi"}}, sessions)

	form := url.Values{}
	form.Set("question", "will this work?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "aabbccdd-0000-4000-8000-000000000003"})

	// nonFlushingWriter explicitly does not satisfy http.Flusher.
	nfw := &nonFlushingWriter{rr: httptest.NewRecorder()}
	srv.ServeHTTP(nfw, req)

	if nfw.Code() != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", nfw.Code(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_HappyPath
// ---------------------------------------------------------------------------

// TestHandleChat_HappyPath verifies the full SSE happy path end-to-end:
//   - the response body contains at least one "event: token" frame for each
//     token emitted by the pipeline,
//   - the response body contains an "event: done" frame that includes the
//     citation source name, and
//   - both the user and the assistant turns are persisted via AppendMessage
//     (two calls: first for the user question, second for the assistant reply).
func TestHandleChat_HappyPath(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	pipeline := &stubPipeline{
		tokens:    []string{"hello", " world"},
		citations: []string{"src.pdf"},
	}
	srv := newTestServer(t, pipeline, sessions)

	form := url.Values{}
	form.Set("question", "what is SRE?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "aabbccdd-0000-4000-8000-000000000004"})

	tf := newTestFlusher()
	srv.ServeHTTP(tf, req)

	body := tf.Body.String()

	// Token frames must be present.
	if !strings.Contains(body, "event: token") {
		t.Errorf("response body missing 'event: token' frame; got:\n%s", body)
	}

	// Done frame must be present and must carry the citation.
	if !strings.Contains(body, "event: done") {
		t.Errorf("response body missing 'event: done' frame; got:\n%s", body)
	}
	if !strings.Contains(body, "src.pdf") {
		t.Errorf("response body missing citation 'src.pdf' in done frame; got:\n%s", body)
	}

	// Both user and assistant turns must have been persisted.
	if len(sessions.appended) != 2 {
		t.Fatalf("AppendMessage called %d time(s), want exactly 2 (user + assistant)", len(sessions.appended))
	}

	userCall := sessions.appended[0]
	if userCall.msg.Role != rag.RoleUser {
		t.Errorf("first AppendMessage call role = %q, want %q", userCall.msg.Role, rag.RoleUser)
	}
	if userCall.msg.Content != "what is SRE?" {
		t.Errorf("first AppendMessage call content = %q, want %q", userCall.msg.Content, "what is SRE?")
	}
	if userCall.citations != nil {
		t.Errorf("user turn must be persisted with nil citations, got %v", userCall.citations)
	}

	assistantCall := sessions.appended[1]
	if assistantCall.msg.Role != rag.RoleAssistant {
		t.Errorf("second AppendMessage call role = %q, want %q", assistantCall.msg.Role, rag.RoleAssistant)
	}
	wantContent := "hello world"
	if assistantCall.msg.Content != wantContent {
		t.Errorf("second AppendMessage call content = %q, want %q", assistantCall.msg.Content, wantContent)
	}
	if len(assistantCall.citations) != 1 || assistantCall.citations[0] != "src.pdf" {
		t.Errorf("second AppendMessage call citations = %v, want [src.pdf]", assistantCall.citations)
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_CreateSessionFails
// ---------------------------------------------------------------------------

// TestHandleChat_CreateSessionFails verifies that when CreateSession returns
// an error the handler emits an "event: error" SSE frame rather than silently
// continuing — the client must always receive an unambiguous error signal when
// session initialisation fails.
func TestHandleChat_CreateSessionFails(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{
		createErr: errors.New("postgres connection refused"),
	}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	form := url.Values{}
	form.Set("question", "will session creation fail?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "aabbccdd-0000-4000-8000-000000000005"})

	tf := newTestFlusher()
	srv.ServeHTTP(tf, req)

	body := tf.Body.String()

	if !strings.Contains(body, "event: error") {
		t.Errorf("response body missing 'event: error' frame when CreateSession fails; got:\n%s", body)
	}
}

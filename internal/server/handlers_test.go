package server

import (
	"context"
	"encoding/json"
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
	tokens     []string
	citations  []string
	err        error
	statusMsgs []string // status messages emitted via onStatus
}

func (p *stubPipeline) Answer(_ context.Context, _ string, _ []rag.Message, _ string, onToken func(string) error, onStatus func(string) error) ([]string, error) {
	if p.err != nil {
		return nil, p.err
	}
	for _, msg := range p.statusMsgs {
		if onStatus != nil {
			if err := onStatus(msg); err != nil {
				return nil, err
			}
		}
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

// validSessionFixture is a well-formed UUID v4 used across handler tests.
const validSessionFixture = "aabbccdd-0000-4000-8000-000000000001"

// ---------------------------------------------------------------------------
// TestHandleIndex
// ---------------------------------------------------------------------------

// TestHandleIndex verifies that GET / renders the chat shell: 200, no Set-Cookie
// header, and no DB call (ListMessages must not be invoked).
func TestHandleIndex(t *testing.T) {
	t.Parallel()

	// A non-nil listErr proves handleIndex does not call ListMessages:
	// if it did, the handler would propagate the error and return 500.
	sessions := &stubSessions{listErr: errors.New("db is on fire")}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Header().Get("Set-Cookie") != "" {
		t.Errorf("expected no Set-Cookie header, got %q", rr.Header().Get("Set-Cookie"))
	}
}

// ---------------------------------------------------------------------------
// TestHandleMessages_BadSessionID
// ---------------------------------------------------------------------------

// TestHandleMessages_BadSessionID verifies that GET /messages with a missing or
// malformed X-Session-ID header is rejected with 400 and does not touch the DB.
func TestHandleMessages_BadSessionID(t *testing.T) {
	t.Parallel()

	// Setting listErr proves no DB call occurred: a 400 (not 500) confirms it.
	sessions := &stubSessions{listErr: errors.New("should not be called")}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	cases := []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"plain string", "not-a-uuid"},
		{"wrong version nibble", "aabbccdd-0000-3000-8000-000000000001"},
		{"uppercase", "AABBCCDD-0000-4000-8000-000000000001"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/messages", nil)
			if tc.header != "" {
				req.Header.Set(sessionHeader, tc.header)
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestHandleMessages_HappyPath
// ---------------------------------------------------------------------------

// TestHandleMessages_HappyPath verifies that GET /messages returns a JSON array
// of messages with nil citations normalised to [] and existing citations preserved.
func TestHandleMessages_HappyPath(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{
		messages: []StoredMessage{
			{Message: rag.Message{Role: rag.RoleUser, Content: "hi"}, Citations: nil},
			{Message: rag.Message{Role: rag.RoleAssistant, Content: "hello"}, Citations: []string{"a.pdf"}},
		},
	}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	req.Header.Set(sessionHeader, validSessionFixture)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var msgs []messageDTO
	if err := json.NewDecoder(rr.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	// User turn: nil citations must be normalised to an empty slice, not JSON null.
	if msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("msgs[0] = {%q, %q}, want {user, hi}", msgs[0].Role, msgs[0].Content)
	}
	if msgs[0].Citations == nil {
		t.Error("user citations must be normalised to [], got nil")
	}

	// Assistant turn: citations preserved.
	if msgs[1].Role != "assistant" || msgs[1].Content != "hello" {
		t.Errorf("msgs[1] = {%q, %q}, want {assistant, hello}", msgs[1].Role, msgs[1].Content)
	}
	if len(msgs[1].Citations) != 1 || msgs[1].Citations[0] != "a.pdf" {
		t.Errorf("msgs[1].Citations = %v, want [a.pdf]", msgs[1].Citations)
	}
}

// ---------------------------------------------------------------------------
// TestHandleMessages_ListMessagesFails
// ---------------------------------------------------------------------------

// TestHandleMessages_ListMessagesFails verifies that when ListMessages errors the
// handler responds 500 so the client receives an unambiguous failure signal.
func TestHandleMessages_ListMessagesFails(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{listErr: errors.New("db is on fire")}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	req.Header.Set(sessionHeader, validSessionFixture)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_BadSessionID
// ---------------------------------------------------------------------------

// TestHandleChat_BadSessionID verifies that POST /chat with a malformed
// X-Session-ID is rejected with 400 before any DB call is made.
func TestHandleChat_BadSessionID(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	srv := newTestServer(t, &stubPipeline{}, sessions)

	form := url.Values{}
	form.Set("question", "will this work?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(sessionHeader, "not-a-uuid")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if len(sessions.appended) != 0 {
		t.Errorf("AppendMessage called %d time(s), want 0 for bad session ID", len(sessions.appended))
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
	req.Header.Set(sessionHeader, "aabbccdd-0000-4000-8000-000000000002")

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
// does not implement http.Flusher the handler responds with 500.
func TestHandleChat_NoFlusher(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	srv := newTestServer(t, &stubPipeline{tokens: []string{"hi"}}, sessions)

	form := url.Values{}
	form.Set("question", "will this work?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(sessionHeader, "aabbccdd-0000-4000-8000-000000000003")

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
//   - the response body contains at least one "event: token" frame,
//   - the response body contains an "event: done" frame with the citation, and
//   - both user and assistant turns are persisted via AppendMessage.
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
	req.Header.Set(sessionHeader, "aabbccdd-0000-4000-8000-000000000004")

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
// TestHandleChat_StatusEventForwarded
// ---------------------------------------------------------------------------

// TestHandleChat_StatusEventForwarded verifies that pipeline status messages
// are forwarded as "event: status" SSE frames before the token frames.
func TestHandleChat_StatusEventForwarded(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	pipeline := &stubPipeline{
		statusMsgs: []string{"Reading resume.pdf…"},
		tokens:     []string{"answer"},
		citations:  []string{"resume.pdf"},
	}
	srv := newTestServer(t, pipeline, sessions)

	form := url.Values{}
	form.Set("question", "what is anthony's work history?")
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(sessionHeader, "aabbccdd-0000-4000-8000-000000000007")

	tf := newTestFlusher()
	srv.ServeHTTP(tf, req)

	body := tf.Body.String()

	if !strings.Contains(body, "event: status") {
		t.Errorf("response body missing 'event: status' frame; got:\n%s", body)
	}
	if !strings.Contains(body, "Reading resume.pdf") {
		t.Errorf("response body missing status message content; got:\n%s", body)
	}
	// Status must appear before the token frame.
	statusIdx := strings.Index(body, "event: status")
	tokenIdx := strings.Index(body, "event: token")
	if statusIdx >= tokenIdx {
		t.Errorf("status frame must precede token frame; statusIdx=%d tokenIdx=%d", statusIdx, tokenIdx)
	}
}

// ---------------------------------------------------------------------------
// TestHandleChat_CreateSessionFails
// ---------------------------------------------------------------------------

// TestHandleChat_CreateSessionFails verifies that when CreateSession returns an
// error the handler emits an "event: error" SSE frame.
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
	req.Header.Set(sessionHeader, "aabbccdd-0000-4000-8000-000000000005")

	tf := newTestFlusher()
	srv.ServeHTTP(tf, req)

	body := tf.Body.String()

	if !strings.Contains(body, "event: error") {
		t.Errorf("response body missing 'event: error' frame when CreateSession fails; got:\n%s", body)
	}
}

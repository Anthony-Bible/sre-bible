package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubPinger implements Pinger with controllable behavior.
type stubPinger struct {
	err error
}

func (p *stubPinger) Ping(_ context.Context) error {
	return p.err
}

// newTestServerWithPinger builds a *Server under test with a pinger set.
func newTestServerWithPinger(t *testing.T, pinger Pinger) *Server {
	t.Helper()
	srv, err := NewServer(&stubPipeline{}, &stubSessions{}, pinger, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}
	return srv
}

// ---------------------------------------------------------------------------
// TestHandleHealthz
// ---------------------------------------------------------------------------

// TestHandleHealthz_AlwaysOK verifies the liveness probe always returns 200
// regardless of DB state — it must never call the pinger.
func TestHandleHealthz_AlwaysOK(t *testing.T) {
	t.Parallel()

	// Even with a nil pinger, healthz must return 200.
	srv := newTestServerWithPinger(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	body := rr.Body.String()
	if body != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ok"}`)
	}
}

// TestHandleHealthz_NilPingerDoesNotPanic verifies that healthz does not
// attempt to call the pinger and therefore does not panic when pinger is nil.
func TestHandleHealthz_NilPingerDoesNotPanic(t *testing.T) {
	t.Parallel()

	srv := newTestServerWithPinger(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	// If this panics, the test will fail with a panic — that's the right signal.
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// ---------------------------------------------------------------------------
// TestHandleReadyz
// ---------------------------------------------------------------------------

// TestHandleReadyz_NilPinger verifies that readyz returns 503 when no pinger
// has been configured — the server is not ready without a DB connection.
func TestHandleReadyz_NilPinger(t *testing.T) {
	t.Parallel()

	srv := newTestServerWithPinger(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// TestHandleReadyz_PingFails verifies that readyz returns 503 when the
// database ping fails — the server must signal it is not ready to receive traffic.
func TestHandleReadyz_PingFails(t *testing.T) {
	t.Parallel()

	pinger := &stubPinger{err: errors.New("connection refused")}
	srv := newTestServerWithPinger(t, pinger)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// TestHandleReadyz_PingSucceeds verifies that readyz returns 200 and a JSON
// body when the database ping succeeds — the server is ready to serve traffic.
func TestHandleReadyz_PingSucceeds(t *testing.T) {
	t.Parallel()

	pinger := &stubPinger{err: nil}
	srv := newTestServerWithPinger(t, pinger)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	body := rr.Body.String()
	if body != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ok"}`)
	}
}

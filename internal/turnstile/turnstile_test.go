package turnstile_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/turnstile"
)

// newTestVerifier points the verifier at srv instead of the real Cloudflare endpoint.
func newTestVerifier(t *testing.T, srv *httptest.Server) *turnstile.Verifier {
	t.Helper()
	v := turnstile.NewVerifier("test-secret", slog.Default())
	v.SetEndpoint(srv.URL)
	return v
}

func serveJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// --- Contract 1: success=true response returns (true, nil) ---

func TestVerify_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, map[string]any{"success": true})
	}))
	t.Cleanup(srv.Close)

	v := newTestVerifier(t, srv)
	ok, err := v.Verify(context.Background(), "good-token", "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("Verify returned false, want true")
	}
}

// --- Contract 2: success=false response returns (false, nil) ---

func TestVerify_Failure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, map[string]any{
			"success":     false,
			"error-codes": []string{"invalid-input-response"},
		})
	}))
	t.Cleanup(srv.Close)

	v := newTestVerifier(t, srv)
	ok, err := v.Verify(context.Background(), "bad-token", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("Verify returned true, want false")
	}
}

// --- Contract 3: non-200 status returns (false, err) ---

func TestVerify_Non200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	v := newTestVerifier(t, srv)
	ok, err := v.Verify(context.Background(), "any-token", "")
	if err == nil {
		t.Error("expected an error for non-200 response, got nil")
	}
	if ok {
		t.Error("Verify returned true on non-200, want false")
	}
}

// --- Contract 4: malformed JSON returns (false, err) ---

func TestVerify_MalformedJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json at all"))
	}))
	t.Cleanup(srv.Close)

	v := newTestVerifier(t, srv)
	ok, err := v.Verify(context.Background(), "any-token", "")
	if err == nil {
		t.Error("expected an error for malformed JSON, got nil")
	}
	if ok {
		t.Error("Verify returned true on decode error, want false")
	}
}

// --- Contract 5: request failure (server closed) returns (false, err) ---

func TestVerify_NetworkError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, map[string]any{"success": true})
	}))
	srv.Close() // close immediately so all requests fail

	v := newTestVerifier(t, srv)
	ok, err := v.Verify(context.Background(), "any-token", "")
	if err == nil {
		t.Error("expected an error for network failure, got nil")
	}
	if ok {
		t.Error("Verify returned true on network error, want false")
	}
}

// --- Contract 6: request body contains expected fields ---

func TestVerify_RequestBody(t *testing.T) {
	t.Parallel()

	var gotSecret, gotResponse, gotRemoteIP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotSecret = r.FormValue("secret")
		gotResponse = r.FormValue("response")
		gotRemoteIP = r.FormValue("remoteip")
		serveJSON(w, map[string]any{"success": true})
	}))
	t.Cleanup(srv.Close)

	v := newTestVerifier(t, srv)
	_, _ = v.Verify(context.Background(), "my-token", "9.9.9.9")

	if gotSecret != "test-secret" {
		t.Errorf("secret = %q, want %q", gotSecret, "test-secret")
	}
	if gotResponse != "my-token" {
		t.Errorf("response = %q, want %q", gotResponse, "my-token")
	}
	if gotRemoteIP != "9.9.9.9" {
		t.Errorf("remoteip = %q, want %q", gotRemoteIP, "9.9.9.9")
	}
}

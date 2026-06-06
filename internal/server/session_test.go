package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// uuidV4Re is the canonical UUID v4 pattern: version nibble is always '4',
// and the first nibble of the clock_seq_hi byte is always 8, 9, a, or b.
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewSessionID_Format verifies that a single generated ID is a well-formed
// UUID v4 string and that no error is returned.
func TestNewSessionID_Format(t *testing.T) {
	t.Parallel()

	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID() returned unexpected error: %v", err)
	}
	if !uuidV4Re.MatchString(id) {
		t.Errorf("newSessionID() = %q, want a UUID v4 matching %s", id, uuidV4Re)
	}
}

// TestNewSessionID_Uniqueness verifies that 1000 independently generated IDs
// are all distinct — collisions would indicate a broken entropy source.
func TestNewSessionID_Uniqueness(t *testing.T) {
	t.Parallel()

	const n = 1000
	seen := make(map[string]struct{}, n)

	for i := range n {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID() iteration %d returned unexpected error: %v", i, err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newSessionID() produced duplicate ID %q after %d generations", id, i)
		}
		seen[id] = struct{}{}
	}

	if len(seen) != n {
		t.Errorf("expected %d unique IDs, got %d", n, len(seen))
	}
}

// TestSessionFromRequest_Present verifies that sessionFromRequest returns the
// cookie value when a "session_id" cookie is present on the request.
func TestSessionFromRequest_Present(t *testing.T) {
	t.Parallel()

	const wantID = "abc12345-0000-4000-8000-000000000000"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: wantID})

	got := sessionFromRequest(req)
	if got != wantID {
		t.Errorf("sessionFromRequest() = %q, want %q", got, wantID)
	}
}

// TestSessionFromRequest_Absent verifies that sessionFromRequest returns the
// empty string when no "session_id" cookie is present on the request.
func TestSessionFromRequest_Absent(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)

	got := sessionFromRequest(req)
	if got != "" {
		t.Errorf("sessionFromRequest() = %q, want empty string when cookie is absent", got)
	}
}

// TestSetSessionCookie_WritesHeader verifies the observable contract of
// setSessionCookie: the response must carry a Set-Cookie header that includes
// the session ID value, the HttpOnly directive, and a Max-Age greater than zero.
func TestSetSessionCookie_WritesHeader(t *testing.T) {
	t.Parallel()

	const sessionID = "deadbeef-dead-4eef-beef-deadbeefdead"

	rr := httptest.NewRecorder()
	setSessionCookie(rr, sessionID)

	// Parse the cookies written to the recorder's response.
	resp := rr.Result()
	defer resp.Body.Close()

	setCookieHeader := rr.Header().Get("Set-Cookie")
	if setCookieHeader == "" {
		t.Fatal("setSessionCookie() wrote no Set-Cookie header")
	}

	// Verify the session ID value is present in the header.
	if !strings.Contains(setCookieHeader, sessionID) {
		t.Errorf("Set-Cookie header %q does not contain session ID %q", setCookieHeader, sessionID)
	}

	// Verify HttpOnly directive is present (case-insensitive match to be robust
	// against capitalisation differences across Go versions).
	if !strings.Contains(strings.ToLower(setCookieHeader), "httponly") {
		t.Errorf("Set-Cookie header %q is missing HttpOnly directive", setCookieHeader)
	}

	// Verify Max-Age is present and non-zero.  We parse the cookies from the
	// response to get a structured view rather than hand-rolling header parsing.
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("setSessionCookie() produced no parseable cookies in the response")
	}

	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == cookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("setSessionCookie() did not set a cookie named %q", cookieName)
	}
	if found.MaxAge <= 0 {
		t.Errorf("cookie MaxAge = %d, want > 0", found.MaxAge)
	}
}

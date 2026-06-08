package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidSessionID verifies validSessionID against known-good and known-bad inputs.
func TestValidSessionID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"valid v4", "aabbccdd-0000-4000-8000-000000000001", true},
		{"valid v4 alt variant b", "aabbccdd-0000-4000-b000-000000000001", true},
		{"empty", "", false},
		{"non-uuid", "not-a-uuid", false},
		{"uuid v3", "aabbccdd-0000-3000-8000-000000000001", false},
		{"microsoft variant nibble (c)", "aabbccdd-0000-4000-c000-000000000001", false},
		{"uppercase", "AABBCCDD-0000-4000-8000-000000000001", false},
		{"too short", "aabbccdd-0000-4000-8000-00000000000", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := validSessionID(tc.id)
			if got != tc.want {
				t.Errorf("validSessionID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestSessionFromRequest_Present verifies that sessionFromRequest returns the
// X-Session-ID header value when present.
func TestSessionFromRequest_Present(t *testing.T) {
	t.Parallel()

	const wantID = "abc12345-0000-4000-8000-000000000000"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(sessionHeader, wantID)

	got := sessionFromRequest(req)
	if got != wantID {
		t.Errorf("sessionFromRequest() = %q, want %q", got, wantID)
	}
}

// TestSessionFromRequest_Absent verifies that sessionFromRequest returns the
// empty string when X-Session-ID is absent.
func TestSessionFromRequest_Absent(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)

	got := sessionFromRequest(req)
	if got != "" {
		t.Errorf("sessionFromRequest() = %q, want empty string when header is absent", got)
	}
}

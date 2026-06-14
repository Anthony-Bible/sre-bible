package server

import (
	"net/http"
	"testing"
)

// TestNormalizeMethod verifies that standard HTTP methods pass through unchanged
// while arbitrary attacker-supplied method tokens collapse to "other", keeping
// the metric label cardinality bounded.
func TestNormalizeMethod(t *testing.T) {
	t.Parallel()

	standard := []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace,
	}
	for _, m := range standard {
		if got := normalizeMethod(m); got != m {
			t.Errorf("normalizeMethod(%q) = %q, want %q (standard methods pass through)", m, got, m)
		}
	}

	// Arbitrary tokens an attacker can send must all map to the single "other"
	// bucket so they cannot inflate metric cardinality.
	arbitrary := []string{"RND8F3K2", "FOOBAR", "get", "Post", "", "PROPFIND", "x-custom"}
	for _, m := range arbitrary {
		if got := normalizeMethod(m); got != "other" {
			t.Errorf("normalizeMethod(%q) = %q, want %q (non-standard methods clamp)", m, got, "other")
		}
	}
}

package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric"
)

// TestNoopDefault exercises the package-level singleton before Init() to confirm
// every instrument is safe to use without configuration (CLI binaries rely on this).
func TestNoopDefault(t *testing.T) {
	t.Parallel()
	if M == nil {
		t.Fatal("metrics.M is nil")
	}
	ctx := context.Background()
	M.HTTPRequests.Add(ctx, 1, metric.WithAttributes(AttrString("route", "x")))
	M.HTTPInFlight.Add(ctx, 1)
	M.HTTPDuration.Record(ctx, 0.1)
	M.LLMResponsesServed.Add(ctx, 1)
	M.LLMResponsesBlocked.Add(ctx, 1, metric.WithAttributes(AttrString("reason", "model_armor")))
	M.LLMToolCalls.Add(ctx, 1, metric.WithAttributes(AttrString("tool", "list_documents"), AttrString("outcome", "ok")))
	M.RAGRetrievalDuration.Record(ctx, 0.2)
	M.RAGChunksRetrieved.Record(ctx, 8)
	M.IngestSources.Add(ctx, 1, metric.WithAttributes(AttrString("result", "ok")))
	M.IngestChunks.Add(ctx, 12)
	M.IngestDuration.Record(ctx, 2.5, metric.WithAttributes(AttrString("stage", "total")))
	M.TurnstileChecks.Add(ctx, 1, metric.WithAttributes(AttrString("outcome", "pass")))
}

// TestInitExposition runs Init, records a few metrics, and confirms the
// Prometheus scrape handler returns text-format output with the project prefix.
// Init() is sync.Once-guarded so this test must be the only one that calls it.
func TestInitExposition(t *testing.T) {
	handler, shutdown, err := Init(context.Background(), "sre-bible-test", nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if handler == nil {
		t.Fatal("Init returned nil handler")
	}

	ctx := context.Background()
	M.LLMResponsesServed.Add(ctx, 3)
	M.LLMResponsesBlocked.Add(ctx, 1, metric.WithAttributes(AttrString("reason", "model_armor")))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%q", rec.Code, body)
	}
	for _, want := range []string{"sre_bible_llm_responses_served", "sre_bible_llm_responses_blocked"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output missing %q\n---\n%s", want, body)
		}
	}
}

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
		if got := NormalizeMethod(m); got != m {
			t.Errorf("NormalizeMethod(%q) = %q, want %q (standard methods pass through)", m, got, m)
		}
	}

	// Arbitrary tokens an attacker can send must all map to the single "other"
	// bucket so they cannot inflate metric cardinality.
	arbitrary := []string{"RND8F3K2", "FOOBAR", "get", "Post", "", "PROPFIND", "x-custom"}
	for _, m := range arbitrary {
		if got := NormalizeMethod(m); got != "other" {
			t.Errorf("NormalizeMethod(%q) = %q, want %q (non-standard methods clamp)", m, got, "other")
		}
	}
}

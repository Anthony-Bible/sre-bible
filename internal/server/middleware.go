package server

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
)

// metricsMiddleware records HTTP traffic via metrics.M. Route attribution uses
// r.Pattern (Go 1.22+ ServeMux), which collapses to the registered pattern
// rather than the raw URL path, keeping label cardinality bounded.
//
// When the underlying writer satisfies http.Flusher, the wrapper passed
// downstream also satisfies it so SSE streaming continues to work. When it
// does not, the wrapper does not — the chat handler's `w.(http.Flusher)`
// check has to keep returning false for non-flushing writers.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := r.Context()

		metrics.M.HTTPInFlight.Add(ctx, 1)
		defer metrics.M.HTTPInFlight.Add(ctx, -1)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var down http.ResponseWriter = rec
		if _, ok := w.(http.Flusher); ok {
			down = flusherRecorder{rec}
		}
		next.ServeHTTP(down, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		base := []attribute.KeyValue{
			metrics.AttrString("route", route),
			metrics.AttrString("method", normalizeMethod(r.Method)),
		}
		metrics.M.HTTPDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
		metrics.M.HTTPRequests.Add(ctx, 1, metric.WithAttributes(
			append(base, metrics.AttrString("status", strconv.Itoa(rec.status)))...,
		))
	})
}

// normalizeMethod clamps the request method to a bounded allowlist before it is
// used as a metric label. Go's HTTP server accepts any RFC 7230 token as a
// method, so an attacker could otherwise emit unbounded distinct label values
// (one new Prometheus time series each) and grow memory without bound. Anything
// outside the standard set collapses to "other".
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return m
	default:
		return "other"
	}
}

// statusRecorder captures the response status code for the metrics label.
// It does NOT implement http.Flusher — Flusher-ability is added by wrapping
// in flusherRecorder only when the underlying writer is actually flushable.
type statusRecorder struct {
	http.ResponseWriter

	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// flusherRecorder is statusRecorder + http.Flusher, used only when the
// underlying writer already implements Flusher.
type flusherRecorder struct{ *statusRecorder }

func (r flusherRecorder) Flush() { r.ResponseWriter.(http.Flusher).Flush() }

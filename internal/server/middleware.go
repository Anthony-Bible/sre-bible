package server

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
)

// metricsMiddleware records HTTP traffic via metrics.M. Route attribution uses
// r.Pattern (Go 1.22+ ServeMux), which collapses to the registered pattern
// rather than the raw URL path, keeping label cardinality bounded.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := r.Context()

		metrics.M.HTTPInFlight.Add(ctx, 1)
		defer metrics.M.HTTPInFlight.Add(ctx, -1)

		base := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var rw http.ResponseWriter = base
		// Preserve Flusher-ability for SSE: only expose Flush when the underlying
		// writer actually supports it, so chat-handler's w.(http.Flusher) check
		// still returns false for non-flushing writers (tests rely on this).
		if _, ok := w.(http.Flusher); ok {
			rw = &flushingRecorder{statusRecorder: base}
		}
		next.ServeHTTP(rw, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		attrs := metric.WithAttributes(
			metrics.AttrString("route", route),
			metrics.AttrString("method", r.Method),
		)
		metrics.M.HTTPDuration.Record(ctx, time.Since(start).Seconds(), attrs)
		metrics.M.HTTPRequests.Add(ctx, 1, metric.WithAttributes(
			metrics.AttrString("route", route),
			metrics.AttrString("method", r.Method),
			metrics.AttrString("status", strconv.Itoa(base.status)),
		))
	})
}

// statusRecorder wraps http.ResponseWriter to capture the response status code
// for the metrics label. It deliberately does NOT implement http.Flusher to make
// SSE streaming a compile-time concern — see the Flush passthrough below.
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

// flushingRecorder is statusRecorder + Flusher passthrough, used only when the
// underlying writer is already a Flusher. Keeping the Flush method off
// statusRecorder itself preserves the chat handler's ability to detect
// non-flushing writers via a plain type assertion.
type flushingRecorder struct {
	*statusRecorder
}

func (r *flushingRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

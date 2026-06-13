// Package metrics centralises OpenTelemetry metric instruments for the project.
//
// All instruments hang off a single *Metrics value exposed as the package-level
// singleton M. Adding a new metric is three steps:
//
//  1. Add a field to the Metrics struct.
//  2. Initialise it in newMetrics() with the appropriate meter.* constructor.
//  3. At the call site: metrics.M.YourCounter.Add(ctx, 1, metric.WithAttributes(...)).
//
// Before Init() is called M is backed by a no-op meter provider, so call sites
// in CLI binaries and tests do not need to special-case "metrics not configured".
// Init() swaps in a Prometheus-exporting meter provider and returns the scrape
// handler the caller wires onto its HTTP listener.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// Metrics holds every instrument the project exports. Fields are populated once
// at construction; their .Add / .Record methods are safe for concurrent use.
type Metrics struct {
	meter metric.Meter

	// HTTP (recorded by server middleware).
	HTTPRequests metric.Int64Counter       // attrs: route, method, status
	HTTPDuration metric.Float64Histogram   // attrs: route, method, unit: s
	HTTPInFlight metric.Int64UpDownCounter // currently-serving requests ("live")

	// LLM / RAG outcomes.
	LLMResponsesServed  metric.Int64Counter     // successful streamed answers
	LLMResponsesBlocked metric.Int64Counter     // attr: reason ("model_armor", "turnstile")
	LLMErrors           metric.Int64Counter     // attr: stage ("embed","search","stream")
	LLMDuration         metric.Float64Histogram // wall-clock, attr: outcome

	// Tool loop.
	LLMToolCalls metric.Int64Counter // attrs: tool, outcome

	// Follow-up suggestion cards (inactivity-triggered).
	FollowUpSuggestions metric.Int64Counter // attr: status ("ok","empty","blocked","unverified","error")

	// Retrieval.
	RAGRetrievalDuration metric.Float64Histogram
	RAGChunksRetrieved   metric.Int64Histogram

	// Ingestion (cmd/ingest).
	IngestSources  metric.Int64Counter     // attr: result
	IngestChunks   metric.Int64Counter     // total chunks produced
	IngestDuration metric.Float64Histogram // attr: stage

	// Turnstile.
	TurnstileChecks metric.Int64Counter // attr: outcome
}

// M is the global metrics singleton. It is non-nil at all times: before Init is
// called it routes through a no-op meter, so call sites never need a nil check.
//
//nolint:gochecknoglobals // the package is a deliberate singleton; M is its public face.
var M *Metrics

// initOnce guards Init against double-registration if a caller invokes it twice.
//
//nolint:gochecknoglobals // guards the one-time Init swap of the global M.
var initOnce sync.Once

// AttrString is a convenience alias for attribute.String to keep call sites terse.
//
//	metrics.M.LLMToolCalls.Add(ctx, 1,
//	    metric.WithAttributes(metrics.AttrString("tool", "list_documents")))
//
//nolint:gochecknoglobals // a const-like alias; attribute.String has no const form.
var AttrString = attribute.String

// init populates M with no-op instruments so the package is usable from t=0.
// Init() can later swap meter providers; the M pointer is replaced atomically.
func init() {
	m, err := newMetrics(noop.NewMeterProvider().Meter("sre-bible"))
	if err != nil {
		// noop meter cannot fail; panic is appropriate because the noop fallback
		// is the floor of the system.
		panic(fmt.Sprintf("metrics: noop init failed: %v", err))
	}
	M = m
}

// Init configures the Prometheus exporter and replaces M with live instruments.
// It returns the http.Handler that serves the Prometheus exposition format
// (mount it at /metrics on whatever listener you choose) and a shutdown function
// to flush and stop the provider.
//
// serviceName feeds the resource attribute exposed alongside every metric.
// log receives non-fatal startup messages.
//
// Safe to call multiple times; only the first call has effect. The returned
// handler from subsequent calls is the same handler.
func Init(ctx context.Context, serviceName string, log *slog.Logger) (http.Handler, func(context.Context) error, error) {
	var (
		handler  http.Handler
		shutdown func(context.Context) error
		initErr  error
	)
	initOnce.Do(func() {
		if log == nil {
			log = slog.Default()
		}
		registry := prometheus.NewRegistry()
		exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
		if err != nil {
			initErr = fmt.Errorf("create prometheus exporter: %w", err)
			return
		}
		res := resource.NewSchemaless(semconv.ServiceName(serviceName))
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(exporter),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)

		m, err := newMetrics(mp.Meter("sre-bible"))
		if err != nil {
			initErr = fmt.Errorf("register instruments: %w", err)
			return
		}
		M = m
		handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry})
		shutdown = mp.Shutdown
		log.InfoContext(ctx, "metrics provider initialised", slog.String("service", serviceName))
	})
	if initErr != nil {
		return nil, nil, initErr
	}
	return handler, shutdown, nil
}

// newMetrics constructs every instrument against the given meter. New metrics
// belong here — add the field above and the constructor below.
//
// instrument count and splitting it would only scatter the registry.
//
//nolint:funlen // a flat instrument-registration list; length scales with the
func newMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{meter: meter}
	var err error

	if m.HTTPRequests, err = meter.Int64Counter(
		"sre_bible_http_requests",
		metric.WithDescription("Total HTTP requests served, labelled by route, method, and status."),
	); err != nil {
		return nil, err
	}
	if m.HTTPDuration, err = meter.Float64Histogram(
		"sre_bible_http_request_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("HTTP request handler latency."),
	); err != nil {
		return nil, err
	}
	if m.HTTPInFlight, err = meter.Int64UpDownCounter(
		"sre_bible_http_requests_in_flight",
		metric.WithDescription("HTTP requests currently being handled."),
	); err != nil {
		return nil, err
	}

	if m.LLMResponsesServed, err = meter.Int64Counter(
		"sre_bible_llm_responses_served",
		metric.WithDescription("Successful LLM responses streamed to the user."),
	); err != nil {
		return nil, err
	}
	if m.LLMResponsesBlocked, err = meter.Int64Counter(
		"sre_bible_llm_responses_blocked",
		metric.WithDescription("LLM requests refused before generation, labelled by reason."),
	); err != nil {
		return nil, err
	}
	if m.LLMErrors, err = meter.Int64Counter(
		"sre_bible_llm_errors",
		metric.WithDescription("Errors during the RAG pipeline, labelled by stage."),
	); err != nil {
		return nil, err
	}
	if m.LLMDuration, err = meter.Float64Histogram(
		"sre_bible_llm_response_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("End-to-end Pipeline.Answer latency."),
	); err != nil {
		return nil, err
	}

	if m.LLMToolCalls, err = meter.Int64Counter(
		"sre_bible_llm_tool_calls",
		metric.WithDescription("Agentic tool invocations, labelled by tool and outcome."),
	); err != nil {
		return nil, err
	}

	if m.FollowUpSuggestions, err = meter.Int64Counter(
		"sre_bible_followup_suggestions",
		metric.WithDescription("Follow-up suggestion-card generations, labelled by status (ok/empty/blocked/unverified/error)."),
	); err != nil {
		return nil, err
	}

	if m.RAGRetrievalDuration, err = meter.Float64Histogram(
		"sre_bible_rag_retrieval_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("pgvector chunk search latency."),
	); err != nil {
		return nil, err
	}
	if m.RAGChunksRetrieved, err = meter.Int64Histogram(
		"sre_bible_rag_chunks_retrieved",
		metric.WithDescription("Chunks returned per retrieval call."),
	); err != nil {
		return nil, err
	}

	if m.IngestSources, err = meter.Int64Counter(
		"sre_bible_ingest_sources",
		metric.WithDescription("Sources ingested, labelled by result."),
	); err != nil {
		return nil, err
	}
	if m.IngestChunks, err = meter.Int64Counter(
		"sre_bible_ingest_chunks",
		metric.WithDescription("Chunks produced and stored during ingestion."),
	); err != nil {
		return nil, err
	}
	if m.IngestDuration, err = meter.Float64Histogram(
		"sre_bible_ingest_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Ingest stage latency."),
	); err != nil {
		return nil, err
	}

	if m.TurnstileChecks, err = meter.Int64Counter(
		"sre_bible_turnstile_checks",
		metric.WithDescription("Turnstile verifications attempted, labelled by outcome."),
	); err != nil {
		return nil, err
	}

	return m, nil
}

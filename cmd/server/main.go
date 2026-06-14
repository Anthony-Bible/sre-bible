package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/email"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	"github.com/Anthony-Bible/sre-bible/internal/llm/openaicompat"
	"github.com/Anthony-Bible/sre-bible/internal/metrics"
	"github.com/Anthony-Bible/sre-bible/internal/modelarmor"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/ratelimit"
	"github.com/Anthony-Bible/sre-bible/internal/server"
	"github.com/Anthony-Bible/sre-bible/internal/turnstile"
)

// compile-time assertions: placed here to avoid import cycles between db/rag and server.
var (
	_ server.SessionRepository = (*db.SessionStore)(nil)
	_ server.Answerer          = (*rag.Pipeline)(nil)
	_ server.Pinger            = (*pgxpool.Pool)(nil)
	_ email.ContactRepository  = (*db.ContactStore)(nil)
	_ rag.EmailSender          = (*email.BoundSender)(nil)
	_ rag.JobMatcher           = (*rag.Matcher)(nil)
	_ rag.Judge                = (*llm.Judge)(nil)
	_ server.TurnstileVerifier = (*turnstile.Verifier)(nil)
	_ rag.InterviewStateStore  = (*db.SessionStore)(nil)
	_ rag.PromptSanitizer      = (*modelarmor.Client)(nil)
	_ server.Suggester         = (*rag.Pipeline)(nil)
	_ rag.FollowUpSuggester    = (*llm.Client)(nil)
	_ rag.FollowUpSuggester    = (*openaicompat.Suggester)(nil)
)

func main() {
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	if err := run(log); err != nil {
		log.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
}

// serverConfig holds the env-derived configuration for the HTTP server. Required
// values are validated in loadServerConfig; optional values carry their defaults.
type serverConfig struct {
	dbURL        string
	geminiKey    string
	anthropicKey string
	model        string
	addr         string
	metricsAddr  string
	serviceName  string
}

// loadServerConfig reads and validates configuration from the environment. The three
// credential vars are fatal when unset; the rest fall back to sensible defaults.
func loadServerConfig() (serverConfig, error) {
	cfg := serverConfig{
		dbURL:        os.Getenv("DATABASE_URL"),
		geminiKey:    os.Getenv("GEMINI_API_KEY"),
		anthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
		model:        os.Getenv("CLAUDE_MODEL"),
		addr:         os.Getenv("LISTEN_ADDR"),
		metricsAddr:  os.Getenv("METRICS_LISTEN_ADDR"),
		serviceName:  os.Getenv("OTEL_SERVICE_NAME"),
	}
	switch {
	case cfg.dbURL == "":
		return cfg, fmt.Errorf("DATABASE_URL is required")
	case cfg.geminiKey == "":
		return cfg, fmt.Errorf("GEMINI_API_KEY is required")
	case cfg.anthropicKey == "":
		return cfg, fmt.Errorf("ANTHROPIC_API_KEY is required")
	}
	if cfg.model == "" {
		cfg.model = "claude-haiku-4-5-20251001"
	}
	if cfg.addr == "" {
		cfg.addr = ":8080"
	}
	if cfg.metricsAddr == "" {
		cfg.metricsAddr = ":9090"
	}
	if cfg.serviceName == "" {
		cfg.serviceName = "sre-bible"
	}
	return cfg, nil
}

func run(log *slog.Logger) error {
	cfg, err := loadServerConfig()
	if err != nil {
		return err
	}

	turnstileSiteKey, tsVerifier, err := setupTurnstile(log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scrapeHandler, metricsShutdown, err := metrics.Init(ctx, cfg.serviceName, log)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer func() {
		if metricsShutdown == nil {
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsShutdown(sctx); err != nil {
			log.WarnContext(sctx, "metrics shutdown", slog.Any("err", err))
		}
	}()

	pool, err := db.NewPool(ctx, cfg.dbURL, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	geminiClient, err := gemini.NewClient(ctx, cfg.geminiKey, log)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	sourceStore := db.NewSourceStore(pool, log)
	sessionStore := db.NewSessionStore(pool, log)
	llmClient := llm.NewClient(cfg.anthropicKey, cfg.model, rag.BaseSystemPrompt, rag.DefaultPersonas(), log)

	suggester, err := setupFollowUpSuggester(llmClient, log)
	if err != nil {
		return err
	}

	var emailerFactory rag.EmailerFactory
	if err := setupEmailer(ctx, pool, log, &emailerFactory); err != nil {
		return fmt.Errorf("setup emailer: %w", err)
	}

	armor, err := setupModelArmor(ctx, log)
	if err != nil {
		return err
	}

	matcher := rag.NewMatcher(geminiClient, sourceStore)
	judge := llm.NewJudge(cfg.anthropicKey, log)
	pipeline := rag.NewPipeline(geminiClient, sourceStore, llmClient, sourceStore, sourceStore, matcher, emailerFactory, 0, log, rag.WithPromptSanitizer(armor), rag.WithFollowUpSuggester(suggester), rag.WithJudge(judge))

	suggestLimiter := setupSuggestLimiter(ctx, log)

	srv, err := server.NewServer(pipeline, sessionStore, pool, tsVerifier, turnstileSiteKey, suggestLimiter, log)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	return serveHTTP(ctx, stop, srv, scrapeHandler, cfg.addr, cfg.metricsAddr, log)
}

// serveHTTP runs the public chat listener and the metrics listener concurrently and
// blocks until one exits or ctx is cancelled, then gracefully tears down both.
func serveHTTP(ctx context.Context, stop context.CancelFunc, srv http.Handler, scrapeHandler http.Handler, addr, metricsAddr string, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv,
		// ReadHeaderTimeout guards against slowloris attacks.
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout closes idle keep-alive connections.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is intentionally omitted — SSE streams are long-lived.
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", scrapeHandler)
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.InfoContext(ctx, "server listening", slog.String("addr", addr))
	log.InfoContext(ctx, "metrics listening", slog.String("addr", metricsAddr))

	errCh := make(chan error, 2)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics listener: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		// One listener exited (bind failure, panic, etc.). Tear down the other
		// before returning so it doesn't keep running orphaned.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
		_ = httpSrv.Shutdown(shutdownCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop()
		log.InfoContext(ctx, "shutting down", slog.String("reason", ctx.Err().Error()))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// setupEmailer wires the contact-email tool when all required env vars are present.
// Writes the factory into out; leaves it nil (feature disabled) when any required var is missing.
func setupEmailer(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, out *rag.EmailerFactory) error {
	emailFrom := strings.TrimSpace(os.Getenv("EMAIL_FROM"))
	emailTo := strings.TrimSpace(os.Getenv("EMAIL_TO"))
	awsRegion := strings.TrimSpace(os.Getenv("AWS_REGION"))
	awsAccessKey := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID"))
	awsSecretKey := strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))

	if emailFrom == "" || emailTo == "" || awsRegion == "" || awsAccessKey == "" || awsSecretKey == "" {
		log.InfoContext(ctx, "contact email tool disabled: EMAIL_FROM, EMAIL_TO, AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY must all be set to enable")
		return nil
	}

	globalLimit := 24
	if s := strings.TrimSpace(os.Getenv("EMAIL_RATE_LIMIT_PER_HOUR")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			log.WarnContext(ctx, "invalid EMAIL_RATE_LIMIT_PER_HOUR, using default",
				slog.String("value", s),
				slog.Int("default", globalLimit),
			)
		} else {
			globalLimit = n
		}
	}

	sesTx, err := email.NewSESTransport(ctx, email.SESConfig{
		Region:    awsRegion,
		AccessKey: awsAccessKey,
		SecretKey: awsSecretKey,
	})
	if err != nil {
		return fmt.Errorf("create SES transport: %w", err)
	}

	contactStore := db.NewContactStore(pool, log)
	emailSvc := email.NewService(contactStore, sesTx, email.Config{
		From:        emailFrom,
		To:          emailTo,
		GlobalLimit: globalLimit,
		Window:      time.Hour,
	}, log)

	log.InfoContext(ctx, "contact email tool enabled",
		slog.String("from", emailFrom),
		slog.String("to", emailTo),
	)
	*out = func(sid string) rag.EmailSender { return emailSvc.Bind(sid) }
	return nil
}

// setupModelArmor reads the required MODEL_ARMOR_TEMPLATE env var and returns a
// configured prompt sanitizer that gates inbound questions against Model Armor's
// prompt-injection / jailbreak filters. The template is fatal — a missing value
// fails startup, mirroring Turnstile. Authentication uses Application Default
// Credentials (ADC), distinct from Gemini's API key.
func setupModelArmor(ctx context.Context, log *slog.Logger) (rag.PromptSanitizer, error) {
	template := strings.TrimSpace(os.Getenv("MODEL_ARMOR_TEMPLATE"))
	if template == "" {
		return nil, fmt.Errorf("MODEL_ARMOR_TEMPLATE is required")
	}
	client, err := modelarmor.NewClient(ctx, template, log)
	if err != nil {
		return nil, fmt.Errorf("create model armor client: %w", err)
	}
	log.InfoContext(ctx, "model armor prompt gate enabled", slog.String("template", template))
	return client, nil
}

// setupFollowUpSuggester picks the provider for inactivity-triggered follow-up
// question cards. The default is the Anthropic llmClient (same model as the main
// chat path). Setting FOLLOWUP_BASE_URL switches to any OpenAI-compatible endpoint
// (OpenRouter, vLLM, Ollama …/v1, LM Studio); FOLLOWUP_MODEL is then required and
// FOLLOWUP_API_KEY is optional (local servers may need no auth).
func setupFollowUpSuggester(llmClient *llm.Client, log *slog.Logger) (rag.FollowUpSuggester, error) {
	base := os.Getenv("FOLLOWUP_BASE_URL")
	if base == "" {
		return llmClient, nil
	}
	model := os.Getenv("FOLLOWUP_MODEL")
	if model == "" {
		return nil, fmt.Errorf("FOLLOWUP_MODEL is required when FOLLOWUP_BASE_URL is set")
	}
	var extraBody map[string]any
	if raw := os.Getenv("FOLLOWUP_EXTRA_BODY"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &extraBody); err != nil {
			return nil, fmt.Errorf("FOLLOWUP_EXTRA_BODY must be a JSON object: %w", err)
		}
	}
	log.Info("follow-up suggestions using OpenAI-compatible endpoint",
		slog.String("base_url", base), slog.String("model", model),
		slog.Int("extra_body_keys", len(extraBody)))
	return openaicompat.New(base, os.Getenv("FOLLOWUP_API_KEY"), model, extraBody, log), nil
}

// setupSuggestLimiter builds the in-process rate limiter for POST /suggestions
// from FOLLOWUP_RATE_LIMIT_PER_HOUR (global hourly cap, default 100) and
// FOLLOWUP_MIN_INTERVAL_MS (per-session min interval, default 4000 — matching the
// client's FOLLOWUP_DELAY_MS). Invalid values fall back to the default and warn,
// mirroring EMAIL_RATE_LIMIT_PER_HOUR.
func setupSuggestLimiter(ctx context.Context, log *slog.Logger) *ratelimit.Limiter {
	globalLimit := envPositiveInt(ctx, "FOLLOWUP_RATE_LIMIT_PER_HOUR", 100, log)
	intervalMS := envPositiveInt(ctx, "FOLLOWUP_MIN_INTERVAL_MS", 4000, log)
	interval := time.Duration(intervalMS) * time.Millisecond
	log.InfoContext(ctx, "suggestion rate limit enabled",
		slog.Int("per_hour", globalLimit),
		slog.Duration("min_interval", interval),
	)
	return ratelimit.New(interval, globalLimit)
}

// envPositiveInt reads key as a positive integer, returning def (and warning)
// when unset, unparseable, or non-positive.
func envPositiveInt(ctx context.Context, key string, def int, log *slog.Logger) int {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		log.WarnContext(ctx, "invalid value, using default",
			slog.String("var", key),
			slog.String("value", s),
			slog.Int("default", def),
		)
		return def
	}
	return n
}

// setupTurnstile reads the two required Turnstile env vars and returns the site key
// and a configured verifier. Both vars are fatal — missing either fails startup.
func setupTurnstile(log *slog.Logger) (string, *turnstile.Verifier, error) {
	siteKey := os.Getenv("TURNSTILE_SITE_KEY")
	if siteKey == "" {
		return "", nil, fmt.Errorf("TURNSTILE_SITE_KEY is required")
	}
	secret := os.Getenv("TURNSTILE_SECRET_KEY")
	if secret == "" {
		return "", nil, fmt.Errorf("TURNSTILE_SECRET_KEY is required")
	}
	return siteKey, turnstile.NewVerifier(secret, log), nil
}

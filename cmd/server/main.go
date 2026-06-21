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

	"github.com/Anthony-Bible/sre-bible/internal/cache"
	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/email"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	"github.com/Anthony-Bible/sre-bible/internal/llm/openaicompat"
	applog "github.com/Anthony-Bible/sre-bible/internal/log"
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
	_ server.SessionRepository = (*cache.CachingSessionStore)(nil)
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
	log := applog.New(os.Stderr, os.Getenv("LOG_FORMAT"), applog.ParseLevel(os.Getenv("LOG_LEVEL")))

	if err := run(log); err != nil {
		log.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
}

// serverConfig holds the env-derived configuration for the HTTP server. Required
// values are validated in loadServerConfig; optional values carry their defaults.
type serverConfig struct {
	dbURL            string
	geminiKey        string
	anthropicKey     string
	model            string
	addr             string
	metricsAddr      string
	serviceName      string
	interviewEnabled bool
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

// cacheConfig pairs the session-cache kill-switch with its resolved settings. When
// enabled is false, cfg is unused and the tier is never constructed.
type cacheConfig struct {
	enabled bool
	cfg     cache.Config
}

// loadCacheConfig reads the SESSION_CACHE_* environment. When SESSION_CACHE_ENABLED
// is false (the default), it returns immediately with enabled=false and nothing else
// is parsed — the feature stays fully inert. When enabled, MY_POD_IP is required
// (fatal if unset, mirroring the TURNSTILE_* pattern) and the remaining knobs fall
// back to their documented defaults. It is parsed inside run (not loadServerConfig)
// because the env helpers need the signal ctx + logger, matching INTERVIEW_MODE_ENABLED.
func loadCacheConfig(ctx context.Context, log *slog.Logger) (cacheConfig, error) {
	if !envBool(ctx, "SESSION_CACHE_ENABLED", false, log) {
		return cacheConfig{enabled: false}, nil
	}

	podIP := strings.TrimSpace(os.Getenv("MY_POD_IP"))
	if podIP == "" {
		return cacheConfig{}, fmt.Errorf("MY_POD_IP is required when SESSION_CACHE_ENABLED=true")
	}

	listenAddr := envString("SESSION_CACHE_LISTEN_ADDR", ":9091")
	headlessDNS := envString("SESSION_CACHE_HEADLESS_DNS", "sre-bible-headless.sre-bible.svc.cluster.local")
	maxBytes := envPositiveInt(ctx, "SESSION_CACHE_MAX_BYTES", 10_000_000, log)
	ttlSeconds := envPositiveInt(ctx, "SESSION_CACHE_TTL_SECONDS", 3600, log)
	refreshSeconds := envPositiveInt(ctx, "SESSION_CACHE_PEER_REFRESH_SECONDS", 30, log)

	return cacheConfig{
		enabled: true,
		cfg: cache.Config{
			SelfIP:      podIP,
			ListenAddr:  listenAddr,
			MaxBytes:    int64(maxBytes),
			TTL:         time.Duration(ttlSeconds) * time.Second,
			PeerRefresh: time.Duration(refreshSeconds) * time.Second,
			HeadlessDNS: headlessDNS,
		},
	}, nil
}

// setupSessionCache builds the optional galaxycache tier in front of store. When the
// cache is disabled it returns the raw store, a nil aux listener, and a no-op cleanup —
// nothing is constructed and no goroutine/listener is started. When enabled it registers
// the stats metrics, starts the peer-refresh goroutine, and returns the read-through
// decorator, an auxListener for the peer endpoint, and a cleanup func that shuts the
// universe down. The caller adds the listener to serveHTTP's aux slice and defers cleanup.
func setupSessionCache(ctx context.Context, store *db.SessionStore, log *slog.Logger) (server.SessionRepository, *auxListener, func(), error) {
	settings, err := loadCacheConfig(ctx, log)
	if err != nil {
		return nil, nil, nil, err
	}
	if !settings.enabled {
		return store, nil, func() {}, nil
	}

	tier, err := cache.New(settings.cfg, store, log)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create session cache: %w", err)
	}
	if err := tier.RegisterMetrics(); err != nil {
		return nil, nil, nil, fmt.Errorf("register session cache metrics: %w", err)
	}
	go tier.RefreshPeers(ctx)

	cleanup := func() {
		if err := tier.Close(); err != nil {
			log.WarnContext(ctx, "session cache shutdown", slog.Any("err", err))
		}
	}
	log.InfoContext(ctx, "session cache enabled",
		slog.String("listen", settings.cfg.ListenAddr),
		slog.String("headless_dns", settings.cfg.HeadlessDNS),
		slog.Duration("ttl", settings.cfg.TTL),
		slog.Duration("peer_refresh", settings.cfg.PeerRefresh),
	)
	listener := &auxListener{name: "cache peer", addr: settings.cfg.ListenAddr, handler: tier.Handler()}
	return tier.Store(), listener, cleanup, nil
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

	metricsHandler, metricsCleanup, err := setupMetrics(ctx, cfg.serviceName, log)
	if err != nil {
		return err
	}
	defer metricsCleanup()

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

	// Optional galaxycache session-state cache tier (kill-switch: SESSION_CACHE_ENABLED).
	// When enabled, sessionRepo is the read-through decorator wrapping sessionStore and
	// cacheListener carries the peer endpoint to run as a third listener; when disabled,
	// sessionRepo is sessionStore itself and cacheListener is nil (behaviour bit-for-bit
	// unchanged). Either way all session access — including interview state — flows
	// through sessionRepo. NOTE: this must run AFTER setupMetrics: when enabled it calls
	// tier.RegisterMetrics, which binds the cache's observable counters to the global
	// meter that metrics.Init swaps in; registering before Init would silently bind them
	// to the no-op meter and the sre_bible_session_cache_* stats would never export.
	sessionRepo, cacheListener, cacheCleanup, err := setupSessionCache(ctx, sessionStore, log)
	if err != nil {
		return err
	}
	defer cacheCleanup()

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
	chatLimiter := setupChatLimiter(ctx, log)

	// Interview Mode is off unless explicitly enabled (opt-in kill-switch). When
	// disabled, the backend refuses to activate it and the frontend hides the command.
	cfg.interviewEnabled = envBool(ctx, "INTERVIEW_MODE_ENABLED", false, log)

	// Quick-DB-phase deadline: bounds the pre-stream session/history DB ops so a saturated
	// pool sheds 503 (acquire-wait load-shed) instead of piling up. Shorter than the DB-side
	// statement_timeout so the context fires first. Invalid/non-positive → default + warn.
	quickDBTimeout := time.Duration(envPositiveInt(ctx, "DB_QUICK_TIMEOUT_MS", server.DefaultQuickDBTimeoutMS, log)) * time.Millisecond

	srv, err := server.NewServer(pipeline, sessionRepo, pool, tsVerifier, turnstileSiteKey, suggestLimiter, chatLimiter, cfg.interviewEnabled, quickDBTimeout, log)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	// Assemble the auxiliary listeners: metrics always, the cache peer endpoint
	// only when the session cache is enabled (cacheListener is nil otherwise).
	aux := []auxListener{{name: "metrics", addr: cfg.metricsAddr, handler: metricsHandler}}
	if cacheListener != nil {
		aux = append(aux, *cacheListener)
	}

	return serveHTTP(ctx, stop, srv, cfg.addr, aux, log)
}

// auxListener is a secondary HTTP listener that runs beside the public chat
// server — the metrics scrape endpoint and, when the session cache is enabled,
// the galaxycache peer endpoint. serveHTTP starts each entry and tears it down
// on shutdown; the caller assembles the slice, so an absent cache means one
// fewer entry rather than a special case here.
type auxListener struct {
	name    string
	addr    string
	handler http.Handler
}

// serveHTTP runs the public chat listener plus every auxiliary listener in aux
// concurrently, blocking until one exits or ctx is cancelled, then gracefully
// tears all of them down. aux is whatever the caller assembled (metrics always,
// the cache peer endpoint only when enabled) — there is no per-listener
// special-casing in here.
func serveHTTP(ctx context.Context, stop context.CancelFunc, srv http.Handler, addr string, aux []auxListener, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv,
		// ReadHeaderTimeout guards against slowloris attacks.
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout closes idle keep-alive connections.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is intentionally omitted — SSE streams are long-lived.
	}

	auxSrvs := make([]*http.Server, len(aux))
	for i, a := range aux {
		auxSrvs[i] = &http.Server{
			Addr:              a.addr,
			Handler:           a.handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	log.InfoContext(ctx, "server listening", slog.String("addr", addr))
	for _, a := range aux {
		log.InfoContext(ctx, a.name+" listening", slog.String("addr", a.addr))
	}

	shutdownAux := func(shutdownCtx context.Context) {
		for _, s := range auxSrvs {
			_ = s.Shutdown(shutdownCtx)
		}
	}

	errCh := make(chan error, 1+len(auxSrvs))
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()
	for i := range auxSrvs {
		s, name := auxSrvs[i], aux[i].name
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s listener: %w", name, err)
			}
		}()
	}

	select {
	case err := <-errCh:
		// One listener exited (bind failure, panic, etc.). Tear down the others
		// before returning so they don't keep running orphaned.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownAux(shutdownCtx)
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
		shutdownAux(shutdownCtx)
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

	globalLimit := envPositiveInt(ctx, "EMAIL_RATE_LIMIT_PER_HOUR", 24, log)

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
// from FOLLOWUP_RATE_LIMIT_PER_HOUR (global hourly cap, default 1000) and
// FOLLOWUP_MIN_INTERVAL_MS (per-session min interval, default 4000 — matching the
// client's FOLLOWUP_DELAY_MS). The global cap is a per-replica abuse backstop, set
// well above realistic concurrent legitimate load so the per-session cooldown
// stays the primary control. Invalid values fall back to the default and warn,
// mirroring EMAIL_RATE_LIMIT_PER_HOUR.
func setupSuggestLimiter(ctx context.Context, log *slog.Logger) *ratelimit.Limiter {
	globalLimit := envPositiveInt(ctx, "FOLLOWUP_RATE_LIMIT_PER_HOUR", 1000, log)
	intervalMS := envPositiveInt(ctx, "FOLLOWUP_MIN_INTERVAL_MS", 4000, log)
	interval := time.Duration(intervalMS) * time.Millisecond
	log.InfoContext(ctx, "suggestion rate limit enabled",
		slog.Int("per_hour", globalLimit),
		slog.Duration("min_interval", interval),
	)
	return ratelimit.New(interval, globalLimit)
}

// setupChatLimiter builds the in-process rate limiter for POST /chat — the most
// expensive endpoint (Model Armor + embedding + pgvector search + streamed
// Anthropic generation with an agentic tool loop) and a DB-pool-starvation risk.
// Built from CHAT_RATE_LIMIT_PER_HOUR (global hourly cap, default 500 — lower than
// suggestions' 1000 since chat is heavier) and CHAT_MIN_INTERVAL_MS (per-session
// min interval, default 5000 — a human conversation turn takes many seconds). The
// global cap is a per-replica abuse backstop; the per-session cooldown stays the
// primary control. Invalid values fall back to the default and warn, mirroring
// setupSuggestLimiter.
func setupChatLimiter(ctx context.Context, log *slog.Logger) *ratelimit.Limiter {
	globalLimit := envPositiveInt(ctx, "CHAT_RATE_LIMIT_PER_HOUR", 500, log)
	intervalMS := envPositiveInt(ctx, "CHAT_MIN_INTERVAL_MS", 5000, log)
	interval := time.Duration(intervalMS) * time.Millisecond
	log.InfoContext(ctx, "chat rate limit enabled",
		slog.Int("per_hour", globalLimit),
		slog.Duration("min_interval", interval),
	)
	return ratelimit.New(interval, globalLimit)
}

// envString reads key as a trimmed string, returning def when unset or blank.
func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envBool reads key as a boolean (strconv.ParseBool syntax: 1/t/true, 0/f/false),
// returning def when unset and def + a warning when unparseable.
func envBool(ctx context.Context, key string, def bool, log *slog.Logger) bool {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		log.WarnContext(ctx, "invalid value, using default",
			slog.String("var", key),
			slog.String("value", s),
			slog.Bool("default", def),
		)
		return def
	}
	return v
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

// setupMetrics initialises the Prometheus-exporting meter provider and returns a
// handler serving the scrape endpoint at GET /metrics plus a cleanup func that
// flushes and stops the provider on shutdown (a no-op when Init returned no
// shutdown hook).
func setupMetrics(ctx context.Context, serviceName string, log *slog.Logger) (http.Handler, func(), error) {
	scrapeHandler, metricsShutdown, err := metrics.Init(ctx, serviceName, log)
	if err != nil {
		return nil, nil, fmt.Errorf("init metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", scrapeHandler)
	cleanup := func() {
		if metricsShutdown == nil {
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsShutdown(sctx); err != nil {
			log.WarnContext(sctx, "metrics shutdown", slog.Any("err", err))
		}
	}
	return mux, cleanup, nil
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

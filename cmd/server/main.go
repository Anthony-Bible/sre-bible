package main

import (
	"context"
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
	"github.com/Anthony-Bible/sre-bible/internal/rag"
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
	_ server.TurnstileVerifier = (*turnstile.Verifier)(nil)
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

func run(log *slog.Logger) error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is required")
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY is required")
	}

	turnstileSiteKey, tsVerifier, err := setupTurnstile(log)
	if err != nil {
		return err
	}

	model := os.Getenv("CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, dbURL, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	geminiClient, err := gemini.NewClient(ctx, geminiKey, log)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	sourceStore := db.NewSourceStore(pool, log)
	sessionStore := db.NewSessionStore(pool, log)
	llmClient := llm.NewClient(anthropicKey, model, rag.SystemPrompt, log)

	var emailerFactory rag.EmailerFactory
	if err := setupEmailer(ctx, pool, log, &emailerFactory); err != nil {
		return fmt.Errorf("setup emailer: %w", err)
	}

	matcher := rag.NewMatcher(geminiClient, sourceStore)
	pipeline := rag.NewPipeline(geminiClient, sourceStore, llmClient, sourceStore, sourceStore, matcher, emailerFactory, 0, log)

	srv, err := server.NewServer(pipeline, sessionStore, pool, tsVerifier, turnstileSiteKey, log)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv,
		// ReadHeaderTimeout guards against slowloris attacks.
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout closes idle keep-alive connections.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is intentionally omitted — SSE streams are long-lived.
	}

	log.Info("server listening", slog.String("addr", addr))

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop()
		log.Info("shutting down", slog.String("reason", ctx.Err().Error()))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
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

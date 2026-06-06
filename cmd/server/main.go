package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/server"
)

// compile-time assertions: placed here to avoid import cycles between db/rag and server.
var (
	_ server.SessionRepository = (*db.SessionStore)(nil)
	_ server.Answerer          = (*rag.Pipeline)(nil)
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

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

	model := os.Getenv("CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx := context.Background()

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
	pipeline := rag.NewPipeline(geminiClient, sourceStore, llmClient, 0, log)

	srv, err := server.NewServer(pipeline, sessionStore, log)
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
	return httpSrv.ListenAndServe()
}

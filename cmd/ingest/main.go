package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/ingest"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: ingest <path-or-url> | ingest migrate")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
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

	if os.Args[1] == "migrate" {
		return nil
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is required for ingestion")
	}

	geminiClient, err := gemini.NewClient(ctx, apiKey, log)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	store := db.NewSourceStore(pool, log)
	pipeline := ingest.NewPipeline(geminiClient, geminiClient, geminiClient, geminiClient, ingest.DefaultURLExtractor{}, store, log)

	return pipeline.Run(ctx, os.Args[1])
}

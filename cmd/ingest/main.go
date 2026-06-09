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
		return fmt.Errorf("usage: ingest <path-or-url> | ingest migrate | ingest rechunk [--dry-run]")
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

	store := db.NewSourceStore(pool, log)

	if os.Args[1] == "rechunk" {
		// Reject any unrecognized extra arg rather than silently treating it as a
		// live run — a typo like `rechunk --dryrun` must not destructively re-embed.
		dryRun := false
		if len(os.Args) > 2 {
			if os.Args[2] != "--dry-run" {
				return fmt.Errorf("usage: ingest rechunk [--dry-run]")
			}
			dryRun = true
		}
		return rechunkAll(ctx, log, store, dryRun)
	}

	pipeline, err := buildPipeline(ctx, store, log)
	if err != nil {
		return err
	}

	return pipeline.Run(ctx, os.Args[1])
}

// buildPipeline reads GEMINI_API_KEY and wires the ingest pipeline (the Gemini
// client backs extraction, embeddings, description, and PII screening). Shared by
// the ingest and rechunk paths so the four-way client wiring lives in one place.
func buildPipeline(ctx context.Context, store ingest.SourceRepository, log *slog.Logger) (*ingest.Pipeline, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is required for ingestion")
	}
	geminiClient, err := gemini.NewClient(ctx, apiKey, log)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return ingest.NewPipeline(geminiClient, geminiClient, geminiClient, geminiClient, ingest.DefaultURLExtractor{}, store, log), nil
}

// rechunkStore is the storage port for rechunkAll: it lists every source with
// stored full text and replaces a source's chunks (via the embedded repository).
type rechunkStore interface {
	AllSourcesWithText(ctx context.Context) ([]ingest.Source, error)
	ingest.SourceRepository
}

// rechunkAll repairs every source that has stored full text by re-segmenting it
// with the current chunker. In dry-run mode it only reports the chunk count each
// source would produce — no embeddings are requested and nothing is written, so
// it needs no GEMINI_API_KEY. A live run re-embeds and atomically replaces chunks
// per source, continuing past a failed source and reporting the count at the end.
func rechunkAll(ctx context.Context, log *slog.Logger, store rechunkStore, dryRun bool) error {
	srcs, err := store.AllSourcesWithText(ctx)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	if len(srcs) == 0 {
		log.InfoContext(ctx, "no sources with stored full text to rechunk")
		return nil
	}

	if dryRun {
		for _, src := range srcs {
			log.InfoContext(ctx, "rechunk preview",
				"name", src.Name, "chunks", len(ingest.ChunkText(src.FullText)))
		}
		log.InfoContext(ctx, "dry run complete — no changes written", "sources", len(srcs))
		return nil
	}

	pipeline, err := buildPipeline(ctx, store, log)
	if err != nil {
		return err
	}

	var failed int
	for _, src := range srcs {
		if _, err := pipeline.Rechunk(ctx, src); err != nil {
			failed++
			log.ErrorContext(ctx, "rechunk failed", "name", src.Name, "err", err)
			continue
		}
	}

	log.InfoContext(ctx, "rechunk complete", "sources", len(srcs), "failed", failed)
	if failed > 0 {
		return fmt.Errorf("rechunk: %d of %d sources failed", failed, len(srcs))
	}
	return nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/ingest"
	"github.com/spf13/pflag"
)

const usage = "usage: ingest <path-or-url> | ingest migrate | ingest rechunk [--dry-run]"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	args := os.Args[1:]
	if len(args) == 0 {
		return errors.New(usage)
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

	store := db.NewSourceStore(pool, log)

	switch cmd := args[0]; cmd {
	case "migrate":
		// Migrations already ran above; nothing further to do.
		return nil
	case "rechunk":
		return runRechunk(ctx, log, store, args[1:])
	default:
		// Any other first argument is treated as a path or URL to ingest.
		pipeline, err := buildPipeline(ctx, store, log)
		if err != nil {
			return err
		}
		return pipeline.Run(ctx, cmd)
	}
}

// runRechunk parses the rechunk subcommand's flags and dispatches to rechunkAll.
// pflag's strict parsing rejects unknown flags and stray positional args, so a
// typo like `rechunk --dryrun` errors out rather than silently performing a
// destructive, re-embedding live run.
func runRechunk(ctx context.Context, log *slog.Logger, store rechunkStore, args []string) error {
	fs := pflag.NewFlagSet("rechunk", pflag.ContinueOnError)
	// Surface parse failures through the returned error (logged by main) rather
	// than pflag's own stderr usage dump.
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "report chunk counts without re-embedding or writing")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("ingest rechunk: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("ingest rechunk: unexpected argument %q", fs.Arg(0))
	}
	return rechunkAll(ctx, log, store, *dryRun)
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

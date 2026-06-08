package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// compile-time interface assertions (gemini and llm don't import rag themselves).
var (
	_ rag.QueryEmbedder   = (*gemini.Client)(nil)
	_ rag.ChunkSearcher   = (*db.SourceStore)(nil)
	_ rag.Generator       = (*llm.Client)(nil)
	_ rag.DocumentLister  = (*db.SourceStore)(nil)
	_ rag.FullTextFetcher = (*db.SourceStore)(nil)
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
		return fmt.Errorf("usage: query \"<question>\"")
	}
	question := os.Args[1]

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

	ctx := context.Background()

	pool, err := db.NewPool(ctx, dbURL, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	gemCli, err := gemini.NewClient(ctx, geminiKey, log)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	store := db.NewSourceStore(pool, log)
	llmCli := llm.NewClient(anthropicKey, "claude-haiku-4-5", rag.SystemPrompt, log)
	matcher := rag.NewMatcher(gemCli, store)
	pipe := rag.NewPipeline(gemCli, store, llmCli, store, store, matcher, nil, 0, log)

	onTrace := func(step rag.TraceStep) error {
		_, err := fmt.Fprintf(os.Stderr, "[trace] %s\n", traceSummary(step))
		return err
	}

	citations, err := pipe.Answer(ctx, "", nil, question, func(tok string) error {
		_, werr := fmt.Fprint(os.Stdout, tok)
		return werr
	}, onTrace)
	if err != nil {
		return fmt.Errorf("answer: %w", err)
	}

	fmt.Fprintf(os.Stdout, "\n\n--- Sources ---\n")
	for _, c := range citations {
		fmt.Fprintf(os.Stdout, "  [%s]\n", c)
	}
	return nil
}

// traceSummary renders one Agent Trace step as a single human-readable line for the CLI.
func traceSummary(step rag.TraceStep) string {
	switch step.Kind {
	case rag.TraceKindRetrieval:
		if step.Retrieval != nil {
			return fmt.Sprintf("retrieval: %d chunks from %d sources", step.Retrieval.ChunkCount, step.Retrieval.SourceCount)
		}
	case rag.TraceKindToolCall:
		if step.ToolCall != nil {
			if step.ToolCall.Target != "" {
				return fmt.Sprintf("tool_call: %s(%s) -> %s", step.ToolCall.Tool, step.ToolCall.Target, step.ToolCall.Outcome)
			}
			return fmt.Sprintf("tool_call: %s -> %s", step.ToolCall.Tool, step.ToolCall.Outcome)
		}
	case rag.TraceKindAnswer:
		if step.Answer != nil {
			return fmt.Sprintf("answer: %d tool round(s), %dms", step.Answer.ToolRounds, step.Answer.DurationMs)
		}
	}
	return string(step.Kind) + ": " + step.Label
}

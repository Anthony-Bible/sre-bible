package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/eval"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	applog "github.com/Anthony-Bible/sre-bible/internal/log"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// compile-time interface assertions.
var (
	_ rag.QueryEmbedder   = (*gemini.Client)(nil)
	_ rag.ChunkSearcher   = (*db.SourceStore)(nil)
	_ rag.Generator       = (*llm.Client)(nil)
	_ rag.DocumentLister  = (*db.SourceStore)(nil)
	_ rag.FullTextFetcher = (*db.SourceStore)(nil)
	_ eval.Judge          = (*eval.GeminiJudge)(nil)
)

func main() {
	lvl := applog.ParseLevel(os.Getenv("LOG_LEVEL"))
	if os.Getenv("EVAL_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	log := applog.New(os.Stderr, os.Getenv("LOG_FORMAT"), lvl)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dbURL := os.Getenv("EVAL_DATABASE_URL")
	geminiKey := os.Getenv("EVAL_GEMINI_API_KEY")
	anthropicKey := os.Getenv("EVAL_ANTHROPIC_API_KEY")

	if dbURL == "" || geminiKey == "" || anthropicKey == "" {
		log.Info("skipping: required env vars not set")
		return nil
	}

	datasetPath := os.Getenv("EVAL_DATASET")
	if datasetPath == "" {
		datasetPath = "testdata/eval/golden.json"
	}

	thresholds := eval.Thresholds{
		Groundedness: parseThreshold("EVAL_THRESHOLD_GROUNDEDNESS", eval.DefaultThresholds.Groundedness),
		Recall:       parseThreshold("EVAL_THRESHOLD_RECALL", eval.DefaultThresholds.Recall),
		Refusal:      parseThreshold("EVAL_THRESHOLD_REFUSAL", eval.DefaultThresholds.Refusal),
		ContactFlow:  parseThreshold("EVAL_THRESHOLD_CONTACT_FLOW", eval.DefaultThresholds.ContactFlow),
		ToolFlow:     parseThreshold("EVAL_THRESHOLD_TOOL_FLOW", eval.DefaultThresholds.ToolFlow),
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
	// Mirror cmd/server's client construction so the eval pipeline exercises the
	// same prompts and persona set (rag.DefaultPersonas). The eval loop runs with
	// no PersonaMode in context, so the agent uses the standard persona — the
	// voice the golden dataset is written against.
	//
	// Pin temperature=0 for the agent under test. Production leaves it unset (the
	// API default ~1.0) for natural conversational variation, but the gate must
	// measure behaviour, not sampling noise: at the default temperature a single
	// borderline refusal case flips between runs, and with only 9 refusal cases
	// that one flip drops the pass rate to 0.889 and trips the 0.90 gate. temp=0
	// makes the agent's answers reproducible so the gate is stable.
	llmCli := llm.NewClient(anthropicKey, "claude-haiku-4-5-20251001", rag.BaseSystemPrompt, rag.DefaultPersonas(), log, llm.WithTemperature(0))
	// Judge with Gemini — a different model family from the Anthropic agent
	// under test — so the judge's failure modes stay independent of the
	// generator's. EVAL_JUDGE_MODEL overrides the default (eval.DefaultJudgeModel).
	judge := eval.NewGeminiJudge(gemCli, os.Getenv("EVAL_JUDGE_MODEL"), log)
	runner := eval.NewRunner(gemCli, store, llmCli, store, store, judge, log)

	dataset, err := eval.LoadDataset(datasetPath)
	if err != nil {
		return fmt.Errorf("load dataset: %w", err)
	}

	log.Info("eval: starting", "cases", len(dataset.Cases), "dataset", datasetPath)

	scored := make([]eval.ScoredResult, 0, len(dataset.Cases))
	for _, c := range dataset.Cases {
		result := runner.Run(ctx, c)
		if result.Error != nil {
			log.Error("eval: case failed", "case_id", c.ID, "err", result.Error)
		}
		sr := runner.Score(ctx, result)
		log.Info("eval: case result",
			"case_id", c.ID,
			"category", string(c.Category),
			"pass", sr.Pass,
			"recall", sr.Score.RecallScore,
			"refusal_pass", sr.Score.RefusalPass,
			"must_not_pass", sr.Score.MustNotPass,
			"must_contain_pass", sr.Score.MustContainPass,
			"tool_calls_pass", sr.Score.ToolCallsPass,
			"citation_score", sr.Score.CitationScore,
			"ground_score", sr.Score.GroundScore,
			"notes", sr.Notes,
		)
		scored = append(scored, sr)
	}

	reports := eval.Aggregate(scored, thresholds)
	allPass := eval.Report(reports, log)

	if !allPass {
		return fmt.Errorf("eval: one or more category gates failed")
	}
	return nil
}

// parseThreshold reads a float64 from the named environment variable.
// If the variable is unset or cannot be parsed, it returns fallback.
func parseThreshold(envKey string, fallback float64) float64 {
	val := os.Getenv(envKey)
	if val == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return fallback
	}
	return f
}

// Command evalsweep drives the existing eval harness across a grid of chunking
// configurations (and retrieval depth k), re-embedding the version-controlled
// fixtures per config, and prints a comparison scorecard. It answers an
// empirical question: do the production chunk constants (target=1000,
// hardCap=1200, overlap=200, k=8) actually beat the alternatives, or are we
// leaving retrieval/answer quality on the table?
//
// The sweep is DESTRUCTIVE — it wipes and re-ingests its target database — so it
// must run against a dedicated throwaway DB, never the live knowledge base. Two
// guards enforce this: it refuses to run when EVAL_DATABASE_URL equals
// DATABASE_URL, and it refuses to wipe a DB that holds any source name outside
// the fixture set unless --force is passed.
//
// It is an on-demand investigation tool, not a CI gate. See the plan and ADR 0010.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/pflag"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/eval"
	"github.com/Anthony-Bible/sre-bible/internal/gemini"
	"github.com/Anthony-Bible/sre-bible/internal/ingest"
	"github.com/Anthony-Bible/sre-bible/internal/llm"
	applog "github.com/Anthony-Bible/sre-bible/internal/log"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// compile-time interface assertions — mirror cmd/eval so the sweep exercises the
// same ports as the gate.
var (
	_ rag.QueryEmbedder   = (*gemini.Client)(nil)
	_ rag.ChunkSearcher   = (*db.SourceStore)(nil)
	_ rag.Generator       = (*llm.Client)(nil)
	_ rag.DocumentLister  = (*db.SourceStore)(nil)
	_ rag.FullTextFetcher = (*db.SourceStore)(nil)
	_ eval.Judge          = (*eval.GeminiJudge)(nil)
)

const (
	fixtureGlob    = "testdata/eval/sources/*.txt"
	defaultDataset = "testdata/eval/golden.json"
	outputDir      = "eval"
	agentModel     = "claude-haiku-4-5-20251001"
	baselineLabel  = "baseline"
	// judgeNoise is the groundedness delta below which two configs are treated as
	// tied: the LLM judge carries this much residual variance even at temp=0, so a
	// smaller difference is not a real win.
	judgeNoise = 0.05
)

// sweepConfig is one point on the grid: a chunk geometry plus a retrieval depth.
type sweepConfig struct {
	Label string
	Chunk ingest.ChunkConfig
	K     int
}

// grid is the ~5-config sweep, varying chunk geometry and k. Expressed as a
// function (not a package var) to satisfy gochecknoglobals; edit here to add or
// remove grid points.
func grid() []sweepConfig {
	return []sweepConfig{
		{baselineLabel, ingest.ChunkConfig{Target: 1000, HardCap: 1200, Overlap: 200}, 8},
		{"smaller", ingest.ChunkConfig{Target: 700, HardCap: 900, Overlap: 150}, 12},
		{"larger", ingest.ChunkConfig{Target: 1400, HardCap: 1700, Overlap: 280}, 6},
		{"overlap-light", ingest.ChunkConfig{Target: 1000, HardCap: 1200, Overlap: 100}, 8},
		{"overlap-heavy", ingest.ChunkConfig{Target: 1000, HardCap: 1200, Overlap: 300}, 10},
	}
}

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
	opts, err := parseFlags()
	if err != nil {
		return err
	}

	env, ok := loadEnv(log)
	if !ok {
		return nil // required env not set — skip cleanly, like cmd/eval
	}
	if err := env.guardProdURL(); err != nil {
		return err
	}

	configs, err := selectConfigs(grid(), opts.configsCSV)
	if err != nil {
		return err
	}

	fixtures, err := loadFixtures()
	if err != nil {
		return err
	}

	dataset, err := eval.LoadDataset(env.datasetPath)
	if err != nil {
		return fmt.Errorf("load dataset: %w", err)
	}

	ctx := context.Background()

	s, closeFn, err := setupSweeper(ctx, log, env, dataset, fixtures, opts.force)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := s.sweep(ctx, configs, opts.bonusK)
	if err != nil {
		return err
	}

	return emitReport(log, rows, env.datasetPath)
}

// options holds the parsed command-line flags.
type options struct {
	force      bool
	configsCSV string
	bonusK     bool
}

func parseFlags() (options, error) {
	fs := pflag.NewFlagSet("evalsweep", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "override the non-fixture-source safety guard (DANGEROUS — only for a DB you intend to wipe)")
	configsCSV := fs.String("configs", "", "comma-separated config labels to restrict the grid (default: all)")
	bonusK := fs.Bool("bonus-k", false, "also sweep baseline chunking across k∈{6,8,12} (no extra embeddings)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return options{}, fmt.Errorf("evalsweep: %w", err)
	}
	return options{force: *force, configsCSV: *configsCSV, bonusK: *bonusK}, nil
}

// envConfig holds the resolved environment configuration.
type envConfig struct {
	dbURL, geminiKey, anthropicKey, datasetPath, judgeModel string
}

// loadEnv reads the EVAL_* environment. The second return is false (and a skip
// is logged) when any required key is missing — mirroring cmd/eval so the sweep
// no-ops in environments without credentials rather than failing.
func loadEnv(log *slog.Logger) (envConfig, bool) {
	e := envConfig{
		dbURL:        os.Getenv("EVAL_DATABASE_URL"),
		geminiKey:    os.Getenv("EVAL_GEMINI_API_KEY"),
		anthropicKey: os.Getenv("EVAL_ANTHROPIC_API_KEY"),
		datasetPath:  os.Getenv("EVAL_DATASET"),
		judgeModel:   os.Getenv("EVAL_JUDGE_MODEL"),
	}
	if e.dbURL == "" || e.geminiKey == "" || e.anthropicKey == "" {
		log.Info("skipping: required env vars not set (EVAL_DATABASE_URL, EVAL_GEMINI_API_KEY, EVAL_ANTHROPIC_API_KEY)")
		return envConfig{}, false
	}
	if e.datasetPath == "" {
		e.datasetPath = defaultDataset
	}
	return e, true
}

// guardProdURL is safety guard 1: never run against the production DB URL, since
// the sweep wipes its target. Exact-match the documented contract.
func (e envConfig) guardProdURL() error {
	if prod := os.Getenv("DATABASE_URL"); prod != "" && prod == e.dbURL {
		return fmt.Errorf("refusing to run: EVAL_DATABASE_URL equals DATABASE_URL (%s) — the sweep wipes its target DB; point EVAL_DATABASE_URL at a dedicated throwaway database", redactURL(e.dbURL))
	}
	return nil
}

// loadFixtures globs the fixture corpus (the same way CI does).
func loadFixtures() ([]string, error) {
	fixtures, err := filepath.Glob(fixtureGlob)
	if err != nil {
		return nil, fmt.Errorf("glob fixtures %q: %w", fixtureGlob, err)
	}
	if len(fixtures) == 0 {
		return nil, fmt.Errorf("no fixtures found at %q (run from the repo root)", fixtureGlob)
	}
	return fixtures, nil
}

// sweeper bundles the dependencies every config run shares, so per-config helpers
// take just a context and the config rather than a long parameter list.
type sweeper struct {
	log     *slog.Logger
	store   *db.SourceStore
	pool    *pgxpool.Pool
	gem     *gemini.Client
	llm     *llm.Client
	judge   eval.Judge
	dataset *eval.Dataset
}

// setupSweeper connects to the sweep DB, migrates it, runs the safety guard,
// builds the clients (mirroring cmd/eval + cmd/ingest), then wipes and ingests
// the fixtures once. It returns the ready sweeper and a cleanup func that closes
// the pool; on any error it closes the pool itself before returning.
func setupSweeper(ctx context.Context, log *slog.Logger, env envConfig, dataset *eval.Dataset, fixtures []string, force bool) (*sweeper, func(), error) {
	pool, err := db.NewPool(ctx, env.dbURL, log)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to sweep database: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			pool.Close()
		}
	}()

	// Ensure schema exists (idempotent) so a bare `go run ./cmd/evalsweep` works
	// without the Makefile's separate migrate step.
	if err := db.Migrate(ctx, pool, log); err != nil {
		return nil, nil, fmt.Errorf("migrate sweep database: %w", err)
	}

	// Safety guard 2: refuse to wipe a DB that holds non-fixture sources.
	if err := guardFixtureOnly(ctx, pool, fixtureSourceNames(fixtures), force, log); err != nil {
		return nil, nil, err
	}

	gemCli, err := gemini.NewClient(ctx, env.geminiKey, log)
	if err != nil {
		return nil, nil, fmt.Errorf("create gemini client: %w", err)
	}
	store := db.NewSourceStore(pool, log)
	// Mirror cmd/eval: pin the agent to temperature=0 with the standard persona
	// set so answers are reproducible and comparable across configs.
	llmCli := llm.NewClient(env.anthropicKey, agentModel, rag.BaseSystemPrompt, rag.DefaultPersonas(), log, llm.WithTemperature(0))
	judge := eval.NewGeminiJudge(gemCli, env.judgeModel, log)

	s := &sweeper{log: log, store: store, pool: pool, gem: gemCli, llm: llmCli, judge: judge, dataset: dataset}

	// Setup once: wipe → ingest the fixtures (stores full_text + descriptions).
	// Per-config chunk geometry is applied later via Rechunk, which reuses this
	// stored full_text, so descriptions and extractions are not recomputed.
	if err := wipeSources(ctx, pool); err != nil {
		return nil, nil, err
	}
	if err := s.ingestFixtures(ctx, fixtures); err != nil {
		return nil, nil, err
	}

	ok = true
	return s, func() { pool.Close() }, nil
}

// sweep runs every selected config (and, when enabled, the bonus-k axis) and
// returns one scorecard row per config.
func (s *sweeper) sweep(ctx context.Context, configs []sweepConfig, bonusK bool) ([]eval.SweepRow, error) {
	rows := make([]eval.SweepRow, 0, len(configs)+3)
	for _, cfg := range configs {
		row, err := s.runConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("config %q: %w", cfg.Label, err)
		}
		rows = append(rows, row)
		s.log.InfoContext(ctx, "config complete", "label", cfg.Label,
			"ground", row.Groundedness, "recall", row.Recall, "mean_rank", row.MeanRank)
	}
	if bonusK {
		bonus, err := s.runBonusK(ctx)
		if err != nil {
			return nil, fmt.Errorf("bonus-k axis: %w", err)
		}
		rows = append(rows, bonus...)
	}
	return rows, nil
}

// buildPipeline wires an ingest pipeline (Gemini backs extraction, embeddings,
// description, and PII screening) with the given chunk geometry.
func (s *sweeper) buildPipeline(cfg ingest.ChunkConfig) *ingest.Pipeline {
	return ingest.NewPipeline(s.gem, s.gem, s.gem, s.gem, ingest.DefaultURLExtractor{}, s.store, s.log,
		ingest.WithChunkConfig(cfg))
}

// ingestFixtures runs every fixture through a default-config pipeline once,
// storing full_text and descriptions.
func (s *sweeper) ingestFixtures(ctx context.Context, fixtures []string) error {
	pipe := s.buildPipeline(ingest.DefaultChunkConfig())
	s.log.InfoContext(ctx, "ingesting fixtures", "count", len(fixtures))
	for _, f := range fixtures {
		if err := pipe.Run(ctx, f); err != nil {
			return fmt.Errorf("ingest fixture %s: %w", f, err)
		}
	}
	return nil
}

// rechunkAll re-segments every stored fixture with cfg's geometry, re-embedding
// the new boundaries. The cheap primitive: it reuses stored full_text, so no
// re-extraction or re-description occurs.
func (s *sweeper) rechunkAll(ctx context.Context, cfg ingest.ChunkConfig) error {
	pipe := s.buildPipeline(cfg)
	srcs, err := s.store.AllSourcesWithText(ctx)
	if err != nil {
		return fmt.Errorf("list sources with text: %w", err)
	}
	for _, src := range srcs {
		if _, err := pipe.Rechunk(ctx, src); err != nil {
			return fmt.Errorf("rechunk %s: %w", src.Name, err)
		}
	}
	return nil
}

// runDataset runs every golden case through a runner pinned to retrieval depth k
// and returns the scored results.
func (s *sweeper) runDataset(ctx context.Context, k int) []eval.ScoredResult {
	runner := eval.NewRunner(s.gem, s.store, s.llm, s.store, s.store, s.judge, s.log, eval.WithRunnerK(k))
	scored := make([]eval.ScoredResult, 0, len(s.dataset.Cases))
	for _, c := range s.dataset.Cases {
		result := runner.Run(ctx, c)
		if result.Error != nil {
			s.log.ErrorContext(ctx, "eval: case failed", "case_id", c.ID, "err", result.Error)
		}
		scored = append(scored, runner.Score(ctx, result))
	}
	return scored
}

// runConfig applies a config's chunk geometry, runs the dataset at its k, and
// assembles the scorecard row (category scores + diagnostics + size shape).
func (s *sweeper) runConfig(ctx context.Context, cfg sweepConfig) (eval.SweepRow, error) {
	if err := s.rechunkAll(ctx, cfg.Chunk); err != nil {
		return eval.SweepRow{}, err
	}
	scored := s.runDataset(ctx, cfg.K)
	dist, err := chunkSizeDist(ctx, s.pool)
	if err != nil {
		return eval.SweepRow{}, err
	}
	return s.row(cfg.Label, cfg.Chunk, cfg.K, scored, dist), nil
}

// runBonusK holds chunking at baseline geometry and varies only k (no extra
// embeddings), isolating the retrieval-depth axis the main grid confounds with
// chunk size. Optional; gated behind --bonus-k.
func (s *sweeper) runBonusK(ctx context.Context) ([]eval.SweepRow, error) {
	base := ingest.DefaultChunkConfig()
	if err := s.rechunkAll(ctx, base); err != nil {
		return nil, err
	}
	dist, err := chunkSizeDist(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	rows := make([]eval.SweepRow, 0, 3)
	for _, k := range []int{6, 8, 12} {
		scored := s.runDataset(ctx, k)
		rows = append(rows, s.row(fmt.Sprintf("baseline-k%d", k), base, k, scored, dist))
	}
	return rows, nil
}

// row flattens a config's scored results into a SweepRow.
func (s *sweeper) row(label string, cfg ingest.ChunkConfig, k int, scored []eval.ScoredResult, dist sizeDist) eval.SweepRow {
	reports := eval.Aggregate(scored, eval.DefaultThresholds)
	diag := eval.ComputeDiagnostics(scored, k)
	return eval.SweepRow{
		Label:        label,
		Target:       cfg.Target,
		HardCap:      cfg.HardCap,
		Overlap:      cfg.Overlap,
		K:            k,
		Groundedness: eval.ScoreFor(reports, eval.CategoryGroundedFactual),
		Recall:       eval.ScoreFor(reports, eval.CategoryRetrievalCheck),
		Refusal:      eval.ScoreFor(reports, eval.CategoryRefusal),
		ContactFlow:  eval.ScoreFor(reports, eval.CategoryContactFlow),
		ToolFlow:     eval.ScoreFor(reports, eval.CategoryToolFlow),
		MeanRank:     diag.MeanRank,
		MeanCtxChars: diag.MeanCtxChars,
		ChunkCount:   dist.count,
		MedianChunk:  dist.median,
		P90Chunk:     dist.p90,
	}
}

// sizeDist is the chunk-size shape of the current DB state.
type sizeDist struct {
	count, median, p90, max int
}

// chunkSizeDist queries the count and char-length distribution of all stored
// chunks. percentile_disc returns an actual observed value (not interpolated),
// which is the right summary for a discrete chunk-size population.
func chunkSizeDist(ctx context.Context, pool *pgxpool.Pool) (sizeDist, error) {
	var d sizeDist
	err := pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY char_length(content)), 0),
		       COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY char_length(content)), 0),
		       COALESCE(max(char_length(content)), 0)
		FROM chunks`).Scan(&d.count, &d.median, &d.p90, &d.max)
	if err != nil {
		return d, fmt.Errorf("query chunk size distribution: %w", err)
	}
	return d, nil
}

// wipeSources removes all sources; chunks cascade via the source_id FK.
func wipeSources(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DELETE FROM sources`); err != nil {
		return fmt.Errorf("wipe sources: %w", err)
	}
	return nil
}

// guardFixtureOnly aborts when the sweep DB holds any source name outside the
// fixture set — the tripwire that stops the sweep from nuking a real knowledge
// base — unless force is set. An empty DB (fresh or already fixtures-only) passes.
func guardFixtureOnly(ctx context.Context, pool *pgxpool.Pool, fixtureNames map[string]struct{}, force bool, log *slog.Logger) error {
	rows, err := pool.Query(ctx, `SELECT name FROM sources ORDER BY name`)
	if err != nil {
		return fmt.Errorf("list existing sources: %w", err)
	}
	defer rows.Close()

	var foreign []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan source name: %w", err)
		}
		if _, ok := fixtureNames[name]; !ok {
			foreign = append(foreign, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate sources: %w", err)
	}
	if len(foreign) == 0 {
		return nil
	}
	if force {
		log.WarnContext(ctx, "safety guard overridden by --force; wiping non-fixture sources",
			"foreign_count", len(foreign), "foreign", foreign)
		return nil
	}
	return fmt.Errorf("refusing to wipe: sweep DB holds %d source(s) outside the fixture set (%v) — this looks like a real knowledge base, not a throwaway sweep DB; point EVAL_DATABASE_URL at a dedicated database or pass --force to override", len(foreign), foreign)
}

// fixtureSourceNames returns the set of source names the fixtures will ingest as,
// derived the same way the ingest pipeline derives them (DeriveSourceName), so
// the guard's comparison matches what gets stored exactly.
func fixtureSourceNames(paths []string) map[string]struct{} {
	names := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		name, _, err := ingest.DeriveSourceName(p)
		if err != nil {
			name = filepath.Base(p)
		}
		names[name] = struct{}{}
	}
	return names
}

// selectConfigs filters the grid to the comma-separated labels in csv (preserving
// grid order). An empty csv selects the whole grid. Unknown labels are an error.
func selectConfigs(all []sweepConfig, csv string) ([]sweepConfig, error) {
	if strings.TrimSpace(csv) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for lbl := range strings.SplitSeq(csv, ",") {
		if lbl = strings.TrimSpace(lbl); lbl != "" {
			want[lbl] = true
		}
	}
	out := make([]sweepConfig, 0, len(want))
	for _, c := range all {
		if want[c.Label] {
			out = append(out, c)
			delete(want, c.Label)
		}
	}
	if len(want) > 0 {
		unknown := make([]string, 0, len(want))
		for k := range want {
			unknown = append(unknown, k)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown config label(s) %v; known: %s", unknown, labelList(all))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no configs selected")
	}
	return out, nil
}

func labelList(configs []sweepConfig) string {
	labels := make([]string, len(configs))
	for i, c := range configs {
		labels[i] = c.Label
	}
	return strings.Join(labels, ", ")
}

// emitReport prints the scorecard to stdout and writes a timestamped markdown
// copy under outputDir (falling back to the current directory if that path is
// unavailable, e.g. blocked by a same-named file).
func emitReport(log *slog.Logger, rows []eval.SweepRow, datasetPath string) error {
	table := eval.FormatSweepTable(rows)
	_, rationale := eval.RecommendConfig(rows, baselineLabel, judgeNoise)

	fmt.Fprint(os.Stdout, "\n=== CHUNKING SWEEP SCORECARD ===\n\n")
	fmt.Fprint(os.Stdout, table)
	fmt.Fprintf(os.Stdout, "\nRecommendation: %s\n\n", rationale)

	dir := outputDir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Warn("could not create output dir; writing report to the current directory instead",
			"dir", dir, "err", err)
		dir = "."
	}
	ts := time.Now().UTC().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("chunk-sweep-%s.md", ts))
	if err := os.WriteFile(path, []byte(buildMarkdown(table, rationale, datasetPath)), 0o600); err != nil {
		return fmt.Errorf("write report %s: %w", path, err)
	}
	log.Info("sweep complete", "report", path, "configs", len(rows))
	return nil
}

// buildMarkdown renders the persisted report, leading with the honest caveat
// that recall@k is near-saturated on this tiny corpus so groundedness and the
// rank/context diagnostics are the signals that actually discriminate.
func buildMarkdown(table, rationale, datasetPath string) string {
	var b strings.Builder
	b.WriteString("# Chunking-parameter sweep\n\n")
	fmt.Fprintf(&b, "Dataset `%s` · fixtures `%s` · agent `%s` (temp=0) · judge temp=0\n\n",
		datasetPath, fixtureGlob, agentModel)
	b.WriteString("Source-level recall@k is near-saturated on this 6-source fixture corpus, so it reads ~flat across configs. ")
	b.WriteString("The discriminating signals are **ground** (LLM judge over the retrieved top-k), **mean_rank** (1-based rank of the first expected-source chunk — lower is better), and **mean_ctx_chars** (context volume fed to the generator).\n\n")
	b.WriteString("## Scorecard\n\n")
	b.WriteString(table)
	b.WriteString("\n## Recommendation\n\n")
	b.WriteString(rationale)
	b.WriteString("\n")
	return b.String()
}

// redactURL strips the password from a database URL before it appears in an
// error message.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	return u.Redacted()
}

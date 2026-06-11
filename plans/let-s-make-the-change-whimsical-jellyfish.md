# Plan: Chunking-parameter sweep eval (do we need to turn the knobs?)

## Context

We just centered chunks on the ~1000-char target (bias fix + live rechunk). The
open question now is **empirical**: are `chunkTarget=1000 / chunkHardCap=1200 /
chunkOverlap=200` (and retrieval `top-k=8`) actually the *best* settings, or are
we leaving retrieval/answer quality on the table? Right now we have no way to
measure that — we'd be guessing.

The repo already has a **production-grade eval harness** (`cmd/eval` +
`internal/eval/`, ADR 0010): a 21-case golden dataset, an independent Gemini
LLM-judge, source-level recall@k, and category gates (groundedness ≥0.75,
recall ≥0.80, refusal ≥0.90, contact ≥0.80) running against 6 version-controlled
fixtures (`testdata/eval/sources/*.txt`). That harness measures *one* fixed DB
state.

**Goal:** build a **sweep** that drives the *existing* harness across a grid of
chunking configs (and top-k), re-embedding the fixtures per config, and prints a
comparison scorecard so we can see whether any config beats today's baseline —
and by enough to matter. The sweep is an on-demand investigation tool, not a CI
gate.

**Decisions (confirmed):** vary **chunking + top-k**; **~5-config grid**.

**Honest caveat baked into the plan:** the fixture corpus is tiny (6 sources,
~40 chunks). Source-level recall@8 is therefore near-saturated and will likely
read ~1.00 flat across configs. The real discriminating signal is the
**groundedness judge** (which reads the retrieved top-k) plus two new sweep-only
diagnostics. If the sweep comes back flat, *that is the answer*: the knobs don't
matter for this corpus — leave them.

## What we're building

Four code changes + a command + a Makefile target. All additive; production
ingest/eval paths keep their current behavior (defaults unchanged).

### 1. Parameterize chunk config — `internal/ingest/chunk.go`

The parametric `chunkWithConfig(text, target, hardCap, overlap)` already exists;
only the public `ChunkText` hard-wires the constants. Add a small carrier + an
explicit-config entry point (do **not** change `ChunkText`'s behavior):

```go
type ChunkConfig struct{ Target, HardCap, Overlap int }

var DefaultChunkConfig = ChunkConfig{chunkTarget, chunkHardCap, chunkOverlap}

// ChunkTextWith chunks using an explicit config (zero fields fall back to default).
func ChunkTextWith(text string, cfg ChunkConfig) []string {
    c := cfg.withDefaults() // any non-positive field → DefaultChunkConfig's value
    return chunkWithConfig(text, c.Target, c.HardCap, c.Overlap)
}
```

`ChunkText` stays `chunkWithConfig(text, chunkTarget, chunkHardCap, chunkOverlap)`.

### 2. Thread config through the ingest pipeline — `internal/ingest/pipeline.go`

Add a `chunkCfg ChunkConfig` field (default `DefaultChunkConfig`) and a functional
option, mirroring the `rag` package's `WithOnToolCall`/`WithTemperature` style.
Make `NewPipeline` variadic so the four existing call sites compile unchanged:

```go
type Option func(*Pipeline)
func WithChunkConfig(cfg ChunkConfig) Option { return func(p *Pipeline){ p.chunkCfg = cfg.withDefaults() } }

func NewPipeline(..., log *slog.Logger, opts ...Option) *Pipeline {
    p := &Pipeline{ ..., chunkCfg: DefaultChunkConfig }
    for _, o := range opts { o(p) }
    return p
}
```

In `Run` (pipeline.go:90) and `Rechunk` (pipeline.go:158) replace
`ChunkText(text)` / `ChunkText(src.FullText)` with `ChunkTextWith(text, p.chunkCfg)`.
Production callers pass no option → identical output.

### 3. Make top-k configurable in the eval Runner — `internal/eval/runner.go`

`Runner.Run` constructs its pipeline with `0` (→ default k=8) at runner.go:84.
Add a `k int` field + `WithRunnerK(k int)` option, variadic `NewRunner`, and pass
`r.k` into `rag.NewPipeline`. `cmd/eval` passes no option → k=8, unchanged.

### 4. Sweep-only diagnostics — `internal/eval/sweep.go` (new)

Deterministic helpers computed from the already-recorded ordered chunks
(`ScoredResult.Result.RetrievedChunks` — `Content` + `SourceName` are exported).
No golden.json changes:

- `ExpectedSourceRank(expected []string, retrieved []RetrievedChunkRecord, k int) int`
  — 1-based rank of the first retrieved chunk whose source is expected; `k+1` if
  absent. Lower = better. A *continuous* retrieval signal that survives recall@k
  saturation.
- `RetrievedContextChars(retrieved []RetrievedChunkRecord) int` — total chars fed
  to the generator/judge; surfaces the precision↔recall tradeoff as chunk size
  changes.
- A `SweepRow` aggregator (config → 4 category scores + mean rank + mean ctx
  chars + chunk-count + median/p90 chunk size) and a markdown/table formatter.

Unit-tested in `internal/eval/sweep_test.go` (pure functions, no DB/LLM).

### 5. The sweep command — `cmd/evalsweep/main.go` (new)

Reuses the harness as a library. Flow:

1. **Env:** `EVAL_DATABASE_URL` (the sweep DB — see Isolation), `EVAL_GEMINI_API_KEY`,
   `EVAL_ANTHROPIC_API_KEY`, optional `EVAL_DATASET`, `EVAL_JUDGE_MODEL`. Skips
   with a log line if keys are unset (mirrors `cmd/eval`).
2. **Safety guard (destructive op):** abort unless the target DB is the dedicated
   sweep DB. Refuse if `EVAL_DATABASE_URL == DATABASE_URL` (when both set), and
   before wiping, abort if any existing source name is **not** in the fixture set
   — unless `--force`. This is the tripwire that stops the sweep from nuking the
   live 12-source KB.
3. **Build clients** exactly like `cmd/eval` (gemini, store, llm temp=0 with
   `rag.DefaultPersonas()`, `GeminiJudge`) **plus** an `ingest.Pipeline` wired
   like `cmd/ingest` (PDF/URL/describer/screener) for fixture ingest.
4. **Setup once:** wipe sweep DB → ingest the 6 fixtures (`testdata/eval/sources/*.txt`,
   globbed like CI) via `Pipeline.Run`. This stores `full_text` + descriptions
   once; chunking differs only per config below, so descriptions persist.
5. **Per config** (grid below):
   - Build `ingest.NewPipeline(..., WithChunkConfig(cfg.Chunk))` and `Rechunk`
     every fixture (re-embeds with the new boundaries — the cheap primitive;
     reuses stored `full_text`, no re-extract/re-describe).
   - Build `eval.NewRunner(..., WithRunnerK(cfg.K))`, run all golden cases,
     `Aggregate` → `[]CategoryReport`.
   - Compute diagnostics (rank, ctx chars) + query chunk-size distribution
     (`select count, percentile_disc(0.5/0.9), max from chunks`).
6. **Output:** print a config × metric scorecard to stdout and write
   `eval/chunk-sweep-<ts>.md`. Recommend the config with the best **groundedness**
   that does **not** regress the recall/refusal/contact guardrails; if baseline
   wins or all rows are within judge noise, state plainly: *no tuning needed*.

### 6. `make eval-sweep` — `Makefile`

Chains migrate-then-sweep against the dedicated sweep DB:

```make
eval-sweep:
	DATABASE_URL=$(EVAL_SWEEP_DATABASE_URL) go run ./cmd/ingest migrate
	EVAL_DATABASE_URL=$(EVAL_SWEEP_DATABASE_URL) \
	EVAL_GEMINI_API_KEY=$(GEMINI_API_KEY) EVAL_ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY) \
	go run ./cmd/evalsweep
```

## The config grid (~5, varying chunk *and* k)

| # | Label | target | hardCap | overlap | k |
|---|---|---|---|---|---|
| 1 | baseline | 1000 | 1200 | 200 | 8 |
| 2 | smaller | 700 | 900 | 150 | 12 |
| 3 | larger | 1400 | 1700 | 280 | 6 |
| 4 | overlap-light | 1000 | 1200 | 100 | 8 |
| 5 | overlap-heavy | 1000 | 1200 | 300 | 10 |

Grid lives as a slice in `cmd/evalsweep` (easy to edit). Because top-k is free
(no re-embed), the command may *also* print baseline chunking at k∈{6,8,12} as a
zero-extra-embedding bonus axis. Estimated cost: ~$0.5–1.5, ~5–10 min total.

## Isolation (protect the live KB)

The sweep is **destructive** (wipe + re-ingest fixtures). It must run against a
**dedicated database**, never the live 12-source KB. One-time setup:

```bash
set -a && . ./.env && set +a
# Create a throwaway DB in the same local Postgres (port 5433 per .env):
psql "$DATABASE_URL" -c 'CREATE DATABASE sre_bible_chunksweep;'
# Point the sweep at it (swap the db name in the URL):
export EVAL_SWEEP_DATABASE_URL="${DATABASE_URL%/*}/sre_bible_chunksweep"
```

The `--force`-gated guard (step 5.2) is the backstop if the URL is misconfigured.

## Test handling

- New deterministic helpers (`ExpectedSourceRank`, `RetrievedContextChars`,
  `withDefaults`) get unit tests in `internal/eval/sweep_test.go` and
  `internal/ingest/chunk_test.go`.
- `ChunkTextWith` test: a small target (e.g. 300/400/50) yields more, smaller
  chunks than default and still respects the hard cap and existing invariants
  (full coverage, no mid-word splits) — assert *contracts*, not exact sizes.
- `WithChunkConfig` pipeline test: mirror existing mock-based pipeline tests
  (`pipeline_test.go`) — a non-default config changes `Rechunk`'s segment count.
- Existing `internal/eval` + `internal/ingest` suites must stay green (additive,
  variadic signatures — no existing call site changes).

## Verification

1. **Build + unit:** `go build ./...`; `make test-unit`; `make lint` — all green,
   no new diagnostics.
2. **Live-KB untouched:** the sweep uses `sre_bible_chunksweep`; confirm the main
   DB still has 12 sources afterward — `go run ./cmd/listsources` → 12 sources,
   155 chunks (unchanged).
3. **Smoke (1 config):** run the command with the grid trimmed to baseline only
   (or a `--configs baseline` flag) to confirm wiring before spending on the full
   grid — expect category scores near the ADR-0010 baseline (~1.00 across).
4. **Full sweep:** `make eval-sweep` → scorecard table + `eval/chunk-sweep-*.md`.
   Read it: does any config beat baseline groundedness without regressing the
   guardrails? Is the spread above judge noise? Note close calls warrant a repeat
   run (LLM-judge variance even at temp=0).
5. **Decision output:** the plan succeeds whichever way the data falls — either
   "config X wins, here's the diff to apply" or "baseline is fine, knobs don't
   move the needle on this corpus."

## Rollback

Purely additive: delete `cmd/evalsweep/` + `internal/eval/sweep.go`, revert the
variadic-option additions in `chunk.go` / `pipeline.go` / `runner.go`, drop the
Makefile target. The live KB is never touched (separate DB), so there is no data
to restore. `dropdb sre_bible_chunksweep` cleans up the throwaway DB.

## Out of scope

- Adding the sweep to CI (expensive, investigative — stays local/manual).
- Re-baselining the gate thresholds or editing golden.json.
- Changing production chunk constants — that's a *follow-up* the sweep's result
  may justify, decided from the data, not assumed here.

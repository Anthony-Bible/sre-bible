# ADR 0012 — Chunking Parameters: Keep Defaults (Sweep-Validated)

## Status
Accepted

## Context
After centering chunks on the ~1000-char target (the chunking fix in 45e569b, which collapsed the resume from ~55 degenerate fragments to ~7 coherent chunks), an empirical question remained open: are the production chunking constants — `chunkTarget=1000`, `chunkHardCap=1200`, `chunkOverlap=200`, and retrieval `top-k=8` — actually the *best* settings, or were we leaving retrieval/answer quality on the table? We had no measurement; tuning them would have been guesswork.

The repo already has the eval harness from ADR 0010 (a golden dataset, an independent Gemini LLM-judge, source-level recall@k, and category gates). That harness measures *one* fixed DB state. To answer the tuning question we built a **sweep** (`cmd/evalsweep`, `internal/eval/sweep.go`) that drives the existing harness across a grid of chunking configs and top-k values, re-embedding the fixtures per config and printing a comparison scorecard. The sweep is an **on-demand investigation tool, not a CI gate** — it is destructive (wipe + re-ingest) and runs against a dedicated throwaway database, never the live KB.

### Two sweep-only diagnostics

Source-level recall@k saturates at 1.000 on the small fixture corpus (6 sources, ~20 chunks), so it cannot discriminate between configs. Two continuous diagnostics were added to provide signal that survives that saturation:

- **`mean_rank`** — the average 1-based position at which the *expected* source first appears in the retrieved top-k (sentinel `k+1` if absent). Lower is better. Unlike binary recall, it measures *how high* the correct source ranks over the topical distractor documents.
- **`mean_ctx_chars`** — the total context volume (runes) fed to the generator/judge, surfacing the precision↔recall tradeoff as chunk size and k change.

The judge-scored `ground` (groundedness) remains the metric of record for answer quality; the diagnostics are supporting evidence about retrieval behaviour.

## Decision
**Keep the production chunking constants unchanged: `target=1000`, `hardCap=1200`, `overlap=200`, `top-k=8`.** The sweep found no config that beats the baseline on answer quality by more than LLM-judge noise without a cost that isn't justified.

### Sweep result (2026-06-13, local throwaway DB, agent + judge at temp=0)

| config | target | hardCap | overlap | k | ground | recall | mean_rank | ctx_chars | chunks | median | p90 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **baseline** | 1000 | 1200 | 200 | 8 | **1.000** | 1.000 | 2.60 | 7919 | 19 | 1060 | 1176 |
| smaller | 700 | 900 | 150 | 12 | 1.000 | 1.000 | 2.00 | 8343 | 28 | 747 | 869 |
| larger | 1400 | 1700 | 280 | 6 | **0.917** | 1.000 | 2.20 | 7044 | 15 | 1448 | 1542 |
| overlap-light | 1000 | 1200 | 100 | 8 | 1.000 | 1.000 | 2.50 | 7578 | 18 | 1022 | 1148 |
| overlap-heavy | 1000 | 1200 | 300 | 10 | 1.000 | 1.000 | 2.40 | 9808 | 21 | 1022 | 1168 |

`refusal` and `contact_flow` were 1.000 for every config (omitted above for width).

### What the data says

- **Groundedness is pinned at 1.000 everywhere except `larger`, which *regressed* to 0.917.** Larger chunks (1400/1700) with fewer total chunks (15) put more irrelevant text in each retrieved slot, and the judge scored the resulting answers lower. This is the one directional signal, and it points *away* from increasing chunk size.
- **`smaller` improved `mean_rank` (2.00 vs 2.60) but not answer quality.** Groundedness stayed at the 1.000 ceiling, so the better retrieval rank did not reach the answer. Adopting it would mean +47% more chunks (28 vs 19) and a larger `k=12` — more embeddings stored and more context tokens per query (`ctx_chars` rose to 8343 *despite* smaller chunks) — plus a full re-ingest of all live sources, all for a retrieval-rank improvement the judge could not convert into a better answer.
- **Recall is uninformative on this corpus**, exactly as anticipated: flat 1.000 across all five configs. The 6-source fixture set is too small to stress retrieval.

`RecommendConfig` returned *"no tuning needed: baseline is within judge noise (±0.050) of every non-regressing config; the knobs don't move the needle on this corpus."*

## Consequences

**Positive:**
- The production constants are now an evidence-backed choice rather than an untested default. The decision is reproducible: `make eval-sweep` regenerates the scorecard.
- The sweep harness (`cmd/evalsweep`, `internal/eval/sweep.go`, `WithChunkConfig`/`WithRunnerK` options) is additive and reusable — re-run it whenever the corpus, embedding model, or chunker changes.
- The result is actionable in the negative direction too: **do not increase chunk size** (the `larger` regression), and there is no payoff to shrinking chunks on the current corpus.

**Negative / caveats:**
- **Scoped to a tiny corpus.** This conclusion holds for the 6-source fixture set. On a real 50+ document knowledge base, smaller chunks + higher k might genuinely improve precision in a way that reaches the answer — the sweep cannot prove that here because the corpus is too small to discriminate on recall. Re-run the sweep against a larger fixture set if the live KB grows substantially.
- **Single-run, small-sample judge signal.** `larger`'s 0.917 is one run over ~6 grounded cases at temp=0; a one-case flip moves the number. The *direction* (bigger chunks never helped, sometimes hurt) is consistent enough to act on; the exact value is not. A close call would warrant a repeat run.
- **Diagnostics are not gates.** `mean_rank` and `mean_ctx_chars` inform judgment but are not wired into any pass/fail threshold; reading them is a manual step.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| Tune the constants by intuition | The whole point of the sweep was to stop guessing. No measurement, no change. |
| Adopt `smaller` (700/900/150, k=12) for its better `mean_rank` | A retrieval-rank gain that does not improve groundedness, bought with more chunks, a larger k, more per-query context tokens, and a full re-ingest. Not worth it on this corpus. |
| Adopt `larger` (1400/1700) for fewer chunks / lower cost | Rejected — it is the only config that *regressed* groundedness (0.917). |
| Wire the sweep into CI | Rejected — it is destructive and embedding-heavy. It stays an on-demand local/manual investigation tool (out of scope for ADR 0010's gate). |
| Re-baseline the ADR 0010 gate thresholds from this run | Out of scope. This ADR records a chunking decision, not a gate change; thresholds remain as set in ADR 0010. |

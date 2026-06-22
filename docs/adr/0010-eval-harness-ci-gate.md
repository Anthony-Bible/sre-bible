# ADR 0010 — Agent Eval Harness and CI Quality Gate

## Status
Accepted

## Context
The RAG pipeline and agentic tool loop are non-deterministic by nature. Prior to this ADR, CI ran no functional tests against the full answer pipeline — only a Docker image build. Regressions in retrieval quality, groundedness, or tool-use behavior were invisible until deployed to production and noticed manually.

There was no automated quality feedback loop. A prompt change, a chunking tweak, or a model upgrade could silently degrade the agent's behavior with no signal in CI.

## Decision
Introduce a golden-dataset eval harness (`cmd/eval`, `internal/eval/`) that runs a curated set of question/answer cases through the full RAG pipeline and scores each response across four dimensions. The harness is wired into CI as a dedicated `eval` job. Because the job spends rate-limited LLM-judge API keys, it does **not** auto-run on every push: it is attached to a manual-approval `eval-gate` GitHub Environment (with a required reviewer) and only runs once a reviewer approves the waiting run in the Actions UI. Unapproved runs simply expire; nothing else in the pipeline depends on the eval job.

### Scoring dimensions (SLOs for a non-deterministic system)

| Dimension | Method | What it measures |
|---|---|---|
| `grounded_factual` | LLM judge (Gemini, temp=0) | Whether the answer is grounded in retrieved context and factually consistent with it |
| `retrieval_check` | Recall@k + citation-accuracy assertions | Whether expected source chunks appear in the retrieved set, and whether the answer's returned citations include the expected sources |
| `refusal` | LLM judge (Gemini, temp=0) with keyword-heuristic fallback | Whether out-of-scope questions are correctly refused rather than hallucinated |
| `contact_flow` | Behavioral pass/fail | Whether contact-email tool invocation follows the required confirmation flow, the answer points the user at the real contact channel (`must_contain`), and no PII leaks (`must_not_contain`) |
| `tool_flow` | Tool-presence (`expected_tool_calls`) + recall | Whether the agent actually invokes the tool a question demands (e.g. `match_job_description` for a job-fit request), independent of how well the prose reads |

Per-category averages (judge/recall averages for the score-based categories, pass rate for the rest) are compared against configurable thresholds — the **soft gate**. The job fails if any category falls below its threshold.

### The hard gate (deterministic assertions)

The soft gate has a structural blind spot: it averages. For a score-based category, one case that drops a required tool call or leaks PII can be averaged back above the line by its well-scoring neighbours, so the violation never reaches any gate. Originally this is exactly what happened — the per-case deterministic assertions (`must_not_contain`, `expected_tool_calls`) were computed but only consulted for `refusal`/`contact_flow`, and discarded for the averaged categories.

Every category now also carries a **hard gate** applied uniformly: a category fails outright if *any* of its cases violates an author-declared deterministic assertion — `must_not_contain`, `must_contain`, `expected_tool_calls`, or `expected_citations` — regardless of how high the average sits. Formally:

```
MeetsGate(cat) = (total == 0) || (avgScore >= threshold && !anyHardFail)
```

Each assertion is **skip-when-undeclared**: the must-pass booleans default to true when a case sets no such field, and citation score is `-1` (skip) when no citations are expected. So a case that declares no deterministic assertion can never hard-fail — the hard gate only bites on a real regression against the current (all-passing) baseline, never on a case that simply doesn't use the assertion. The recall and refusal averages keep their existing **soft** treatment (not folded into the hard gate) to avoid flaking on the residual LLM-judge variance.

`expected_citations` is scored by `ScoreCitations` (a clone of recall: fraction of expected citation names present in the answer's returned citation set), and a case hard-fails when that fraction sits below the majority bar `0.5` — mirroring the recall floor. This wires citation accuracy, previously captured but never scored, into the gate.

`tool_flow` is its own category so tool behaviour reports on its own line rather than hiding inside `grounded_factual`'s average. It is a pass-rate gate (threshold `0.80`, effectively must-pass at n=1) and the hard gate independently enforces its `expected_tool_calls` assertion.

### Non-determinism handling

- The LLM judge runs at `temperature=0` for maximum repeatability.
- **The agent under test also runs at `temperature=0` in eval** (production leaves it at the API default). The gate must measure regressions in behaviour, not sampling noise: at the default temperature a single borderline refusal case occasionally answers instead of refusing, and with only nine refusal cases that one flip drops the pass rate to 0.889 — below the 0.90 gate — turning the gate into a coin flip. Pinning the agent to `temperature=0` makes its answers reproducible. A small residual variance remains (the API is not perfectly deterministic even at temp=0, observed as `gf-004` scoring 0.0 vs 0.5 between runs), which the per-category averaging below absorbs.
- The **soft** gate is set on **per-category averages**, not per-case pass/fail, to absorb the small variance that remains even at temp=0. The **hard** gate (above) is exact and per-case — but only on deterministic assertions, which carry no judge variance, so it cannot flake.
- If the judge API call returns an error (rate limit, transient failure), that case is marked `SKIP` rather than `FAIL` so a flaky API does not block the pipeline.

### Threshold rationale

A baseline was re-run against the ingested fixtures on 2026-06-09 (multiple runs of the full golden dataset with the agent pinned to `temperature=0`; results were stable across runs). This baseline includes the distractor documents (see below) and runs on top of the chunking fix in 45e569b, which reduced the resume from ~55 degenerate fragments to ~7 coherent chunks. Observed per-category scores:

| Category | Observed | Threshold | Margin |
|---|---|---|---|
| `grounded_factual` | 1.00 | 0.75 | precautionary; tolerates one grounded case regressing without flaking |
| `retrieval_check` | 1.00 | 0.80 | catches one case failing recall |
| `refusal` | 1.00 | 0.90 | catches a single missed refusal (safety-critical, kept tight) |
| `contact_flow` | 1.00 | 0.80 | catches one case (effectively requires all) |
| `tool_flow` | 1.00 | 0.80 | pass-rate; effectively must-pass at n=1, with the hard gate enforcing the tool call |

The `retrieval_check`, `refusal`, `contact_flow`, and `tool_flow` thresholds sit **just below** their observed 1.00 baseline: low enough to absorb the residual variance a non-deterministic, LLM-judged pipeline carries even at `temperature=0`, high enough to catch a real regression. Groundedness keeps a deliberately wider precautionary margin (see below). These soft thresholds are now backstopped by the per-case hard gate on deterministic assertions, so a single dropped tool call, PII leak, or missed expected citation fails its category outright even if the average would otherwise clear the bar.

#### Distractor documents

`testdata/eval/sources/` carries four distractor documents alongside the two real fixtures: a generic on-call field guide, a fictional other engineer's SRE résumé (different employer, cloud, service mesh, and OSS projects), a vendor-neutral SRE-principles essay, and a Kubernetes observability guide. None names Anthony or contains his specific facts (and none contains an `@`, so they cannot leak into a PII-redaction case). Their purpose is to give `retrieval_check` real discriminating power: before they existed the corpus held only the two sources a query could possibly match, so recall@k was near-tautological. With the distractors in place, the topical competitors *do* surface in the retrieved top-k for the overlapping questions (the on-call guide for the on-call question, the fictional résumé for the open-source question, the principles essay — twice — for the philosophy question), yet the expected source still outranks them every run. Recall staying at 1.00 now means retrieval genuinely preferred Anthony's content over plausible look-alikes, not that there was nothing else to retrieve. Dropping a new `.txt` into the directory is enough to extend the distractor set; CI ingests the whole directory by glob.

Groundedness keeps the widest margin (0.75 vs an observed 1.00) on purpose. All six grounded cases now score 1.0 every run. Earlier baselines saw the sixth case (`gf-004`, "daily-use languages") stably docked to 0.0 whenever the agent gave a fuller answer: the answer was factually correct — Bash, gRPC, and Protocol Buffers are all on the resume's skills line — but the grounding judge scores against the retrieved top-k chunk block, not the whole document, and under the old chunker the resume fragmented into ~55 tiny chunks, so the consolidated skills line often fell out of the retrieved context and the judge flagged the extra-but-true detail as ungrounded. The chunking fix in 45e569b ("stop degenerate chunk staircase") resolved this: the resume now ingests to ~7 coherent chunks, the skills line lands in the retrieved set, and `gf-004` scores 1.0. The 0.75 gate is kept as a precautionary floor — wide enough that a single grounded case regressing (e.g. to a low judge score from residual non-determinism) does not flake the gate, tight enough to fail if two cases regress. It can be tightened toward the 1.00 baseline if a snugger groundedness gate is wanted.

### Judge model

The judge runs on **Gemini** (`gemini-3.1-pro-preview` by default, overridable via `EVAL_JUDGE_MODEL`) at `temperature=0`. This is a deliberate choice: the agent under test is an Anthropic Claude model, so judging with a different vendor's model de-correlates the judge's failure modes from the generator's. A model grading its own output shares its blind spots and tends to rubber-stamp its own mistakes; an independent (and stronger) judge gives a more trustworthy gate signal. `gemini-3.1-pro-preview` is a "thinking" model, so the judge calls budget generous `MaxOutputTokens` to keep reasoning tokens from truncating the small trailing JSON verdict.

### Cost

Approximately $0.10–$0.30 per CI run, assuming ~6 Gemini judge calls per run (one groundedness or refusal call per case) at temperature=0. Because the eval job is gated behind manual approval, this cost is incurred on-approval rather than on every push. Embedding calls (Gemini) and the RAG generation step (Claude) are negligible at this scale.

### Secrets

Two separate repository secrets are used so eval credentials can be scoped independently of production secrets:

- `EVAL_GEMINI_API_KEY` — used for embedding queries, ingesting fixtures, and the LLM judge.
- `EVAL_ANTHROPIC_API_KEY` — used by the RAG generation step (the Claude agent under test).

## Consequences

**Positive:**
- Regressions in retrieval, groundedness, and tool-use behavior are caught in CI before reaching production.
- The golden dataset is a living specification of expected agent behavior.
- Eval fixtures (`testdata/eval/sources/`) — both the real sources and the distractor documents — are version-controlled alongside the code that processes them. CI ingests the whole directory by glob, so adding a fixture needs no workflow change.

**Negative / caveats:**

- **False confidence:** Passing the eval set does not guarantee correctness on arbitrary questions. The golden dataset is a finite sample; novel phrasing or topics not covered will not be caught.
- **Prompt injection surface:** Adversarial cases in the golden set provide minimal coverage. A full red-team suite against prompt injection via ingested sources is deferred.
- **Baseline dependency:** The thresholds are derived from the 2026-06-09 baseline run (see Threshold rationale). They are only as valid as that baseline — the fixture set (including the distractor documents), the judge model (`gemini-3.1-pro-preview`), the agent model, and the agent's pinned `temperature=0` are all inputs, so the dataset must be re-baselined whenever any of them changes, or the gate will measure against a stale bar.
- **Re-baselined for case set 32 → 38 (2026-06-22):** The dataset grew from 32 to 38 cases — a new `tool_flow` category (`tf-001`, recategorized from `gf-007`) plus five new cases: an over-refusal guard (`gf-008`), a positive contact-channel assertion (`cf-004`), a citation-accuracy case (`rc-005`), and three new refusal vectors (fabricated attribution, ungrounded inference of protected attributes, cross-session leakage). A live end-to-end CI run on 2026-06-22 confirmed all five gates pass: grounded_factual 0.90 (7/7), retrieval_check 1.00 (5/5), refusal 1.00 (21/21), contact_flow 1.00 (4/4), tool_flow 1.00 (1/1), with no hard-fails. `cf-004`'s `must_contain` token (`"linkedin"`) matched the agent's real contact wording, and `rc-005` scored citation accuracy 1.00 end-to-end. grounded_factual now baselines at 0.90 rather than the old 1.00: `gf-008` (the Staff/Principal fit guard) scores ~0.80, which sits comfortably above the 0.75 floor but pulls the category average down — the precautionary margin was sized for exactly this.
- **Retrieval-coverage artifact:** Because the grounding judge sees only the retrieved top-k chunks rather than the full source, a factually-correct answer can in principle be scored as ungrounded when relevant detail is fragmented out of the retrieved set. This was observed on `gf-004` under the old degenerate chunker (~55 tiny chunks) and is no longer observed after the chunking fix (45e569b) reduced the resume to ~7 coherent chunks. The risk remains structural for any future source that fragments badly; a fuller fix is feeding the judge the full source for grounded_factual cases.
- **Non-zero cost:** Approved eval runs make live API calls. Cost is low but non-zero and subject to API pricing changes. The manual-approval gate keeps this off the per-push path, but it also means the gate only runs when a reviewer remembers to approve it.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| No eval | Unacceptable — invisible regressions are the entire problem being solved. |
| Eval exists but no CI gate (aspirational-only) | Provides signal but no enforcement; developers would ignore a non-blocking check. |
| Deterministic-only tests (no LLM judge) | Retrieval recall and refusal heuristics can be deterministic, but groundedness — the most important quality dimension — requires semantic judgment that only an LLM judge can provide. |
| Same model (Claude) as both judge and generator | Rejected: a model grading its own output produces correlated errors and self-grading bias. Using Gemini as the judge fully de-correlates the judge from the Anthropic agent under test. |
| Eval auto-runs on every push | Rejected: each run spends rate-limited LLM-judge keys. A manual-approval `eval-gate` Environment keeps the spend on-demand while still letting the gate run per PR when wanted. |

# Plan: `match_job_description` JD-fit tool (Issue #12)

## Context

Recruiters arrive at `sre.bible` with a structured artifact — a Job Description — but
today's Resume Agent forces them to decompose it themselves through a series of
questions. This feature lets a Viewer paste a JD and get back a grounded **Fit
Scorecard**: each JD requirement mapped to cited evidence from Anthony's ingested
Sources, an honest gap assessment per requirement, and an overall fit summary.

Secondarily it is a portfolio demonstration of senior agentic engineering — task
decomposition, per-requirement grounded Retrieval, citation discipline, and honest
structured synthesis — legible to a hiring manager in a single interaction.

GitHub issue: https://github.com/Anthony-Bible/sre-bible/issues/12

This plan **supersedes parts of the issue** where grilling found a better or more
correct design; those deltas are called out per decision below.

## Resolved design decisions (from grilling)

### D1 — Tool input contract: the **model** decomposes, not the tool
Tool input is a pre-extracted `requirements: string[]` produced by Claude during its
normal reasoning — **not** a raw `job_description` string the tool splits. So:
no secondary LLM call, no fragile Go heuristic; decomposition is the strong main
model's and stays visible via `onStatus`; non-JD/ambiguous input is handled naturally
(the model asks for clarification and never calls the tool). The tool is pure
per-requirement retrieval + structured evidence assembly.
*Supersedes* the issue's "tool decomposes the JD" framing, its `fieldJobDescription`
single-string schema, and its ~8000-byte JD-truncation step.

### D2 — Gap framing: single neutral gap + contact nudge (3 match classes)
A requirement with no retrieved evidence is a **Gap**, worded neutrally ("No supporting
evidence in Anthony's documented background") — making **no** claim about whether
Anthony has the skill, in either direction. When any Gap exists, the model appends a
soft CTA inviting the recruiter to ask Anthony directly, routing toward the existing
`send_contact_email` tool. Match classes: **Strong** (clear cited evidence),
**Partial** (related but incomplete cited evidence), **Gap** (no corpus evidence).
*Supersedes* the issue's two-gap-type distinction ("documentation gap" vs "outside
experience"), which would force the model to assert ungrounded knowledge — a violation
of the grounding rule in `prompt.go`.

### D3 — JD persistence: accept-as-is, never log content
The pasted JD persists in session history via the existing `AppendMessage` path, like
any Viewer-volunteered content (consistent with the `CONTEXT.md` Session posture). No
selective redaction, no new disclosure UI for MVP. The tool **never logs** JD or
requirement text — only `requirements_count`, per-req `chunks_found`, `sources_cited`,
`duration_ms`. Chat-message PII redaction beyond the existing ingest-time redaction is
out of scope.

### D4 — Suggested question: replace, don't append
Replace the existing "infrastructure tooling and cloud platforms" prompt (#3) in
`defaultSuggestedQuestions()` with the JD prompt
("Paste a job description and I'll show how Anthony matches it"). Count stays at 5,
honoring the glossary's "3–5" invariant — **no `CONTEXT.md` change**.
*Supersedes* the issue's "add one suggested question" (which would have made 6, breaking
the glossary).

### D5 — Add minimal table CSS (CSS-only UI change)
Add ~10 lines to the existing `<style>` block in `templates/index.html` so the
scorecard table has borders, cell padding, a bold header row, and an
`overflow-x: auto` wrapper (the assistant bubble is capped at 82% width; a 3-column
table otherwise overflows on mobile). No route/SSE/JS-logic change. The streaming
re-parse jank (table rebuilds row-by-row mid-stream; final render is correct) is an
accepted MVP limitation.
*Refines* the issue's "no UI changes for MVP" — verified true that GFM tables render
(`marked.setOptions({gfm:true})`, DOMPurify keeps table tags), but unstyled and
mobile-overflowing without this CSS.

## Terminology (to add to `CONTEXT.md` glossary during implementation)

> Plan mode blocks editing `CONTEXT.md`; these are captured here and written when
> implementation begins.

- **Fit Scorecard** — the structured assistant output mapping each JD Requirement to a
  Match class + cited evidence, plus an overall fit summary. _Avoid_: "fit summary",
  "structured analysis" (use as prose, not as the canonical noun).
- **Requirement** — a single discrete expectation extracted from a Job Description by
  the Resume Agent (e.g. "5+ years Kubernetes in production"). The unit of
  per-requirement Retrieval.
- **Match class** — one of **Strong**, **Partial**, **Gap**, assigned by the Resume
  Agent to each Requirement based on retrieved evidence.
- **Job Description (JD)** — a recruiter-supplied document describing a role; pasted as
  Viewer-volunteered content, not an ingested Source.
- Update the **Tool** entry: it currently says "Three tools exist"; this adds a fourth,
  `match_job_description`.

## Implementation

All paths verified against the live codebase during grilling.

### 1. New interface + adapter (`internal/rag/`)
- `internal/rag/domain.go`: define consumption-site interface
  `JobMatcher { MatchRequirement(ctx, requirement string, k int) ([]RetrievedChunk, error) }`.
  Add `Matcher JobMatcher` field to `ToolSet` (nil = tool not advertised, matching
  `Lister`/`Fetcher`/`Emailer`).
- `internal/rag/matcher.go` (new): `Matcher` wraps `QueryEmbedder` + `ChunkSearcher`;
  `MatchRequirement` calls `EmbedQuery` then `SearchChunks` (k defaults to 4 when ≤0).
  **No LLM call in the adapter.** Constructor `NewMatcher(embedder, searcher)`.

### 2. Pipeline wiring (`internal/rag/pipeline.go`)
- Add `matcher JobMatcher` field to `Pipeline` and a parameter to `NewPipeline`
  (breaking signature change).
- In `Answer`, set `tools.Matcher = p.matcher` when non-nil (mirrors `tools.Emailer`).

### 3. Update all `NewPipeline` call sites (5 total — issue undercounted)
- `cmd/server/main.go:110` — pass `rag.NewMatcher(geminiClient, sourceStore)`.
- `cmd/query/main.go:67` — pass a real matcher (`rag.NewMatcher(gemCli, store)`).
- `internal/rag/pipeline_test.go` (3 sites: ~59, ~160, ~259) — pass a stub/nil matcher.
- Add compile-time assertion `var _ rag.JobMatcher = (*rag.Matcher)(nil)` in
  `cmd/server/main.go` alongside the existing assertions.

### 4. The tool (`internal/llm/llm.go`)
- Constants: `toolMatchJobDescription = "match_job_description"`, `fieldRequirements = "requirements"`.
- `buildToolParams`: register the tool gated on `tools.Matcher != nil`. Input schema:
  required `requirements` (array of strings).
- `runTool`: add `case toolMatchJobDescription` → `runMatchJobDescription`.
- `runMatchJobDescription`:
  - Unmarshal `requirements []string`; empty → tool error.
  - Cap to ~12 requirements; cap each string to ~200 chars (token/DoS bound).
  - `onStatus("Analyzing N requirements…")`, then per-requirement progress
    (`onStatus("Searching for evidence: <requirement>")`).
  - Concurrent retrieval via `errgroup` (cap ~4): `Matcher.MatchRequirement(ctx, req, 4)`.
  - Build structured JSON tool result
    `{requirements:[{requirement, evidence:[{excerpt, source}]}]}`; truncate each
    excerpt to ~300 chars. Empty `evidence` is normal — the model classifies it as Gap.
  - Return the collected source names so they flow to citations (see step 5).
  - Log counts + duration only; **never** requirement/JD text.

### 5. Citation flow refactor (contained to `internal/llm`)
- Change `runTool` return type `(string, bool, string)` → `(string, bool, []string)`.
- `collectToolResults`: append the slice to `fetchedNames`.
- `runFetchFullDocument`: return `[]string{sourceName}`.
- `runMatchJobDescription`: return the deduped per-requirement source names.
- The existing `Pipeline.Answer` citation-dedup loop already merges the list.

### 6. System prompt (`internal/rag/prompt.go`, `SystemPrompt` const)
Add guidance: when a Viewer pastes a JD, extract its distinct Requirements and call
`match_job_description` with them (at most once per turn); render a **Fit Scorecard** as
a GFM Markdown table (Requirement | Match | Evidence) with Strong/Partial/Gap; cite a
specific Source for every evidence item; never fabricate; word Gaps neutrally (no claim
either way) and, when Gaps exist, invite the Viewer to contact Anthony; if the input is
not clearly a JD, ask for clarification instead of emitting a scorecard.

### 7. UI (`internal/server/`)
- `server.go`: replace the tooling prompt in `defaultSuggestedQuestions()` with the JD
  prompt (D4).
- `templates/index.html`: add the table CSS to the existing `<style>` block (D5).

### 8. Dependency + ADR
- Promote `golang.org/x/sync` from indirect → direct in `go.mod` (errgroup import).
- `docs/adr/0008-match-job-description-tool.md` (next number confirmed): record
  model-decomposes-not-tool (D1), model-synthesis over numeric heuristic (distance not
  exposed by `SearchChunks` — verified), single-neutral-gap framing (D2), and the
  citation-flow refactor. Qualifies as an ADR: hard-ish to reverse, surprising vs the
  filed issue, and a real trade-off.

### Out of scope (confirmed)
`book_intro_call` (P2, separate issue), `whats_anthony_doing_now` (P3, via scheduled
ingestion, separate ADR), typed `scorecard` SSE event + dedicated UI component,
`job_scorecards` persistence table, raising `maxToolRounds` (5 is ample for a single
tool call).

## TDD execution workflow (4 agents, per CLAUDE.md)

Implementation runs strict red → green → refactor → review using the four
`dotfiles-dev-tools` TDD subagents. Each unit of work below is a full cycle; do **not**
advance phases until the current one is satisfied. Tests assert the **contract**
(behavior/return shape), never the implementation.

1. **`dotfiles-dev-tools:red-phase-tester`** — write the failing tests first, before any
   production code, for each unit: `rag.Matcher.MatchRequirement` (happy/error/`k≤0`),
   `runMatchJobDescription` (empty→error, >12 truncated, evidence+sources, no-chunks→empty,
   source dedup), `buildToolParams` gating on `ToolSet.Matcher`, and the pipeline
   threading the matcher into `ToolSet`. Confirm every test fails for the right reason.
2. **`dotfiles-dev-tools:green-phase-implementer`** — write the *minimal* code to make
   the red tests pass: the `JobMatcher` interface + `Matcher` adapter, the `ToolSet`
   field + pipeline wiring + call-site updates, the tool constants/schema/`runTool`
   case/`runMatchJobDescription`, and the `(string, bool, []string)` citation refactor.
   Stop as soon as the suite is green.
3. **`dotfiles-dev-tools:tdd-refactor-specialist`** — with tests green, clean up:
   dedupe shared helpers (e.g. excerpt/requirement truncation), tighten the `errgroup`
   concurrency block, align logging/`slog` usage and naming with the existing tool
   handlers, and ensure no behavior change. Re-run the suite after each refactor.
4. **`dotfiles-dev-tools:tdd-review-agent`** — final pass: verify no tests are skipped
   or stubbed-out, no unnecessary mocks, the acceptance criteria from Issue #12 are
   each covered by a test, and the contract (not implementation details) is what's
   asserted. Surface any gap as the next red cycle.

The system-prompt, CSS, suggested-question, ADR, and `go.mod` edits are not test-driven
(prose/markup/config); they are covered by the manual end-to-end Verification below.

## Testing (contract-level, per CLAUDE.md "test the contract")

- `internal/rag/matcher_test.go`: stub embedder/searcher — happy path; error
  propagation from embed and from search; `k≤0` defaults to 4. No real DB.
- `internal/llm` tests: `runMatchJobDescription` — empty requirements → error; >12
  requirements truncated; happy path returns evidence with sources; a no-chunks
  requirement returns empty evidence; returned source names deduped.
- `buildToolParams`: advertises `match_job_description` only when `ToolSet.Matcher`
  non-nil.
- `internal/rag/pipeline_test.go`: pipeline threads the matcher into `ToolSet` and
  leaves it nil when unconfigured.
- Run: `make test-unit`; full DB suite with `TEST_DATABASE_URL=... go test ./... -count=1`.
- `make lint` (go vet) and confirm LSP diagnostics are clean after edits.

## Verification (end-to-end)

1. `make db-up && make migrate` (no new migration, but DB must be up).
2. Ingest at least one Source if empty: `make ingest SRC=path/to/resume.pdf`.
3. CLI smoke test through the real pipeline:
   `make query Q="<paste a short JD with 3–4 requirements>"` — confirm the model calls
   the tool, status lines appear, and the output is a scorecard with Strong/Partial/Gap
   + citations, and a Gap reads neutrally.
4. `make serve`, open the chat UI, click the new JD suggested question, paste a JD:
   - status messages appear during processing;
   - scorecard renders as a styled table (check desktop **and** a mobile viewport — it
     should scroll, not overflow);
   - every evidence row cites a Source; gaps invite contact;
   - a deliberately off-corpus requirement shows as a Gap, not a fabricated Partial;
   - pasting non-JD text yields a clarification request, not a nonsense scorecard.
5. Confirm logs show counts/duration only — **no JD or requirement text**.

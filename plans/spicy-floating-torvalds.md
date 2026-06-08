# Act I — Persistent Agent Trace + Citation Grounding Reveal

## Context

`sre.bible` is a RAG agent that answers recruiter questions about Anthony Bible **and** doubles as a portfolio piece demonstrating agentic-engineering skill. Today that skill is **invisible**: the agent's reasoning surfaces only as a transient `status` SSE event rendered in a single `#status-indicator` that shows the latest string and then **vanishes**. The retrieval step emits nothing at all, and the grounding chunk text is discarded after `BuildUserMessage` and never reaches the browser. A recruiter sees streamed prose with no evidence an agent reasoned, escalated, or grounded anything.

**Act I** promotes that ephemeral signal into a **persistent, structured Agent Trace** shown per assistant message as a collapsible timeline (searched → N chunks → decided to fetch resume.pdf → composed answer · 2 tool rounds), and makes **citations clickable** to reveal the exact retrieved chunk excerpt(s) that grounded the answer. The trace survives reload (returned by `GET /messages`) and is visible to the Owner for analytics.

### Locked decisions (from grill-with-docs)
1. **Persist** the trace — full, structured, survives reload.
2. **Structured typed steps** `{kind, label, details}`; kinds: `retrieval`, `tool_call`, `answer`.
3. **Grounding reveal in MVP, persisted** — click citation → retrieved chunk excerpt(s) for that source (source-level; no claim-level mapping exists). Tool-fetched citations → "fetched full document" (no excerpt).
4. **No similarity scores** — chunk_count + source_count + excerpt text only. No `RetrievedChunk`/SQL change.
5. **UI** — per-message native `<details>`/`<summary>` (free a11y). Auto-expand live, auto-collapse to a one-line summary on done, collapsed-by-default on reload.
6. **PII** — curated labels + SAFE specifics (doc names, counts). `send_contact_email` → generic label only, never the Viewer's email/draft. HARD RULE: never the system prompt, the chunk-context prompt, or raw tool result payloads.
7. **Replace `onStatus` with `onTrace`** — remove the `status` SSE event + `#status-indicator`; add a `trace` SSE event.
8. **ADR `0008`** — `docs/adr/0008-persistent-agent-trace.md`. Do NOT renumber issues #12/#13 (deferred to when those acts are worked on).
9. **Glossary** — update `Escalation` + `Citation`; add `Agent Trace`, `Trace Step`, `Grounding Excerpt`.

---

## 1. Trace type — `internal/rag/domain.go`

Lives in `rag` (cycle-free: `rag` imports neither `db` nor `server`; both import `rag`). A single flat `TraceStep` with a `Kind` discriminator and optional pointer detail structs — serializes to one JSONB array, preserves order, and the typed structs act as a PII allow-list by construction.

```go
type TraceStepKind string
const (
    TraceKindRetrieval TraceStepKind = "retrieval"
    TraceKindToolCall  TraceStepKind = "tool_call"
    TraceKindAnswer    TraceStepKind = "answer"
)

type TraceStep struct {
    Kind      TraceStepKind    `json:"kind"`
    Label     string           `json:"label"`
    Retrieval *RetrievalDetail `json:"retrieval,omitempty"`
    ToolCall  *ToolCallDetail  `json:"tool_call,omitempty"`
    Answer    *AnswerDetail    `json:"answer,omitempty"`
}
type RetrievalDetail struct {
    ChunkCount  int                `json:"chunk_count"`
    SourceCount int                `json:"source_count"`
    Excerpts    []GroundingExcerpt `json:"excerpts"` // empty on zero-chunk path
}
type GroundingExcerpt struct {
    SourceName string `json:"source_name"`
    Text       string `json:"text"`
}
type ToolCallDetail struct {
    Tool    string `json:"tool"`
    Target  string `json:"target,omitempty"` // SAFE: doc name only; EMPTY for send_contact_email
    Outcome string `json:"outcome"`          // ok | error | not_found | refused
}
type AnswerDetail struct {
    ToolRounds int   `json:"tool_rounds"`
    DurationMs int64 `json:"duration_ms,omitempty"`
}
```

**PII guard:** only `GroundingExcerpt.Text` carries doc text (intended — same `chunk.Content` already sent to the model). `send_contact_email` → `Target=""`, `Label="Drafted a message to Anthony"`, never the reason/body/email.

---

## 2. DB layer

**Migration `internal/db/migrations/0008_add_trace.sql`** — idempotent, nullable JSONB (NULL distinguishes "no trace" from "empty"):
```sql
-- +goose Up
ALTER TABLE messages ADD COLUMN IF NOT EXISTS trace JSONB;
-- +goose Down
ALTER TABLE messages DROP COLUMN IF EXISTS trace;
```

**`AppendMessage` — extend the signature** (not a new method; trace is per-turn data like `citations`). In `server.SessionRepository` + `db.SessionStore`:
```go
AppendMessage(ctx, sessionID string, msg rag.Message, citations []string, trace []rag.TraceStep) error
```
- Marshal trace to JSON; `len(trace)==0` → pass `[]byte(nil)` so pgx writes SQL `NULL`.
- User-turn call passes `nil` for both citations and trace.

**`ListMessages`** — add `trace` to SELECT, scan into a nullable `[]byte`, unmarshal when `len>0`; on unmarshal error log-and-degrade (`trace=nil`), don't fail the list.

**`server.StoredMessage`** gains `Trace []rag.TraceStep`.

---

## 3. Callback threading

New signatures (drop `onStatus func(string)`, add `onTrace func(TraceStep) error`):
- `rag.Generator.StreamAnswer(ctx, messages, tools, onToken, onTrace) ([]string, error)`
- `rag.Pipeline.Answer(ctx, sessionID, history, question, onToken, onTrace) ([]string, error)` + `server.Answerer`

**`internal/rag/pipeline.go` — `retrieval` step on BOTH paths.** Emit right after `SearchChunks`, **before** the zero-chunk branch, so it fires even on early return. A `buildRetrievalStep(chunks)` helper produces one `GroundingExcerpt` per chunk (source + `Content`), `ChunkCount=len(chunks)`, `SourceCount=unique sources`. This is the only place with chunk `Content` before `BuildUserMessage` consumes it — no DB change. Then thread `onTrace` into `StreamAnswer`.

**`internal/llm/llm.go` — `tool_call` + `answer` steps.**
- `start := time.Now()` at top of `StreamAnswer`.
- Replace `onStatus` with `onTrace` through `collectToolResults` → `runTool`. Emit one `tool_call` step per tool_use block; reuse the existing curated strings as `Label`; map `Outcome` (ok/error/not_found/refused); `Target`= doc name (empty for email).
- `StreamAnswer` emits the **`answer` step itself** before each non-error `return` (it owns the `round` counter → `ToolRounds`, and `time.Since(start)` → `DurationMs`). Avoids widening the return signature. Pipeline emits `retrieval`; llm emits `tool_call` + `answer`.

**Second call site:** `cmd/query/main.go:74` — swap its `onStatus` for an `onTrace` that prints a one-line summary per step to stderr (else the build breaks).

---

## 4. SSE — `internal/server/sse.go`

- **Remove** `sseStatus` + the `status` event (keep `msgPayload` — still used by `sseError`).
- Add `tracePayload{Step rag.TraceStep}` + `sseTrace(w, f, step)` → `event: trace\ndata: {"step":{...}}\n\n`. (sse.go imports `rag`; server already does.)
- `donePayload` stays `{Citations}` — trace is already in the DOM from streamed `trace` events; `done` just triggers collapse.

---

## 5. Server handler + DTO — `internal/server/handlers.go`

- `handleChat`: replace the `onStatus` closure with `onTrace` that **appends to a `[]rag.TraceStep` slice AND forwards via `sseTrace`**; persist the assistant turn with the accumulated trace (detached `persistCtx` unchanged); user turn → `nil` trace.
- `messageDTO` gains `Trace []rag.TraceStep \`json:"trace"\``; `handleMessages` copies `sm.Trace`, normalizing `nil` → `[]` (mirrors the citations-nil handling).

**Compile-time assertions:** update `cmd/server/main.go` (and the `cmd/query` call site) for the new interface signatures.

---

## 6. Frontend — `internal/server/templates/index.html`

**Markup** (per assistant message): a `<details class="trace">` (with `open` while live; no `open` on reload) holding a `<summary>` one-liner (`🔍 Searched · N chunks · M tool rounds`) + an `<ol>` of steps; plus a `.grounding` panel (hidden) as the excerpt reveal target.

**CSS:** remove the `#status-indicator` block; add `.trace`/`.trace-steps`/`.grounding` styles; make `.citation` `cursor:pointer`; `white-space:pre-wrap; word-break:break-word; max-height` on `.grounding` so it never breaks the 600px mobile layout. Native `<details>` = free keyboard/screen-reader support.

**SSE reader loop (L459-489):** delete the `status` branch + `showStatus`/`clearStatus`; add a `trace` branch that appends a rendered `<li>` to the live panel and updates the summary counts; stash the retrieval step's `excerpts` for the in-flight message. On `done`, finalize: move the trace into the message, remove `open` (collapse), wire citation clicks.

**One step renderer** reused live + on reload: retrieval → counts; tool_call → curated `label` (via `escapeHTML`); answer → `✓ Answered — N tool rounds`.

**Citation reveal:** `finalizeStreamingBubble(text, citations, trace)` finds the `retrieval` step, groups `excerpts` by `source_name`. Pills get `role="button" tabindex="0" aria-expanded`. On click/Enter → render that source's excerpts into `.grounding`, **each via `escapeHTML`** (XSS-critical — excerpts are raw doc text, NOT markdown); no excerpts → "Fetched full document — no excerpt available."

**`restoreHistory` (L278-292):** assistant branch → `finalizeStreamingBubble(m.content, m.citations||[], m.trace||[])`, rendered collapsed; keep the existing per-message try/catch so a corrupt trace skips just that panel. Omit the `<details>` entirely when trace is empty (legacy NULL rows).

---

## 7. ADR + glossary

**`docs/adr/0008-persistent-agent-trace.md`** (match the 0005 format): Context (status transient/lost on reload, escalation invisible after the fact) → Decision (structured `TraceStep` persisted as JSONB; replace `onStatus`/`status` with `onTrace`/`trace`; source-level grounding reveal; PII allow-list) → Relationship to ADR 0005 (supersedes its transient-status stance) → Consequences (transparency/reproducibility vs. JSONB growth + excerpt text duplicated into `messages`).

**`CONTEXT.md`** (pure glossary, no impl detail):
- **Update `Escalation`** — now recorded as a `tool_call` Trace Step in the persisted Agent Trace (drop "does not alter session history").
- **Update `Citation`** — now clickable to reveal Grounding Excerpts from that Source.
- **Add** `Agent Trace`, `Trace Step`, `Grounding Excerpt`.

---

## 8. Tests (contract-focused)

- **`internal/rag/pipeline_test.go`**: update `stubGenerator` (`onStatus`→`onTrace`) and all `Answer(...)` calls. Rename `TestPipeline_OnStatusThreaded`→`...OnTraceThreaded`. New: `TestPipeline_RetrievalStepEmitted` (2 sources, 1 dup → one retrieval step, ChunkCount=3, SourceCount=2, excerpts keyed by source); `TestPipeline_ZeroChunkEmitsRetrievalStep` (chunks=nil → retrieval step ChunkCount=0, empty excerpts, generator NOT called, canned token still sent).
- **`internal/llm/llm_test.go`** (StreamAnswer is unmockable — test at the `runTool`/`collectToolResults` seam, as existing `TestRunTool_*` already do; the `nil` 4th arg becomes `onTrace`): new `TestRunTool_EmitsToolCallStep_*` per tool (correct Tool/Outcome/SAFE Target); `TestToolCallStep_EmailHasNoPII` (Target=="", generic label, no email/body/reason anywhere — PII regression guard). Note in-file why the `answer` step isn't unit-tested here (covered via handler + pipeline stub).
- **`internal/db/session_test.go`** (integration, skips without `TEST_DATABASE_URL`): update calls/assertions for the new arg. New: `TestAppendMessage_AssistantWithTrace_RoundTrip`; `TestListMessages_NilTraceDegradesGracefully` (NULL → nil, no error).
- **`internal/server/handlers_test.go`**: update `stubPipeline.Answer` + `stubSessions.AppendMessage`/`appendedCall` for the new arg/capture. Rename `...StatusEventForwarded`→`...TraceEventForwarded` (asserts `event: trace` frames in order AND **no** `event: status`). Update `...HappyPath` (assistant append carries trace, user carries nil). New `TestHandleMessages_TraceReturned` (nil → `[]`).
- **`internal/server/sse_test.go`**: delete `TestSseStatus_Format`; add `TestSseTrace_Format`.

---

## 9. Verification (end-to-end)

1. `make db-up` → `make migrate` (or `make serve`, which auto-migrates 0008).
2. `go vet ./...` (`make lint`) + `go build ./...` — confirms both call sites (`cmd/server`, `cmd/query`) + assertions updated.
3. `make test` — new contracts; db tests skip cleanly without `TEST_DATABASE_URL`.
4. `make ingest SRC=<doc>` then `make query Q="..."` — CLI prints `onTrace` step summaries; pipeline still answers.
5. Browser (`make serve`, `localhost:8080`): ask a question → live trace panel expands & fills → collapses to a one-liner on done. Click a citation → grounding excerpt(s) reveal; tool-fetched-only citation → "fetched full document". Reload → panels restore collapsed, reveal still works. Out-of-scope question → zero-chunk path still shows a `chunk_count=0` retrieval step + canned reply. DevTools shows `event: trace`, **no** `event: status`.
6. Inspect a row: `messages.trace` JSONB present for assistant turns, NULL for user turns; no system prompt / email PII in any stored trace.

---

## Critical files
- `internal/rag/domain.go` — TraceStep types + Generator/Answer signature
- `internal/rag/pipeline.go` — Answer signature + retrieval step (incl. zero-chunk path)
- `internal/llm/llm.go` — tool_call + answer steps; onStatus→onTrace
- `internal/server/handlers.go` — accumulate + persist + sseTrace; messageDTO.trace
- `internal/server/templates/index.html` — `<details>` panel, SSE loop rewrite, citation→grounding reveal
- Also: `internal/db/session.go` + `migrations/0008_add_trace.sql`, `internal/server/server.go` (interface + StoredMessage), `internal/server/sse.go`, `cmd/server/main.go` + `cmd/query/main.go` (call sites/assertions)

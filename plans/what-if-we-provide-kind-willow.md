# Agentic Full-Document Retrieval (list_documents + fetch_full_document tools)

## Context

Chunk-only RAG fails on questions whose answer spans more than the retrieved excerpts. Observed failure: "What is anthony's working history?" → the retrieved resume.pdf chunks didn't contain the full work history, so the agent claimed the info wasn't available — even though the full document has it.

Root cause discovered during exploration: the Gemini-extracted markdown is **discarded after chunking** (`internal/ingest/pipeline.go`); `sources` has no full-text column, and `SearchChunks` returns only chunk content + source name.

**Decisions locked with the user (via /grill-with-docs):**
1. Store full text in a new nullable `sources.full_text` column, populated at ingest.
2. The LLM decides when to escalate: two tools — `list_documents` and `fetch_full_document(source_name)`.
3. Chunks stay as the primary context (top-k=8 unchanged); tools are an escalation path.
4. Tool loop capped at **5 rounds**, then force a final answer via `tool_choice: none`.
5. New `status` SSE event so the UI shows "Reading resume.pdf…" during tool rounds.
6. Always on — no config flag.
7. ADR 0005 documenting the partial reversal of ADR 0001's pure-RAG stance.

Anthropic SDK: `anthropic-sdk-go v1.47.0` (verified in go.mod).

## Changes

### 1. Migration — CREATE `internal/db/migrations/0003_add_full_text.sql`
```sql
-- +goose Up
ALTER TABLE sources ADD COLUMN full_text TEXT;
-- +goose Down
ALTER TABLE sources DROP COLUMN full_text;
```
Nullable so existing rows survive (`full_text = NULL` = legacy row). Auto-applied by the existing embedded `db.Migrate` on startup.

### 2. Ingest — `internal/ingest/domain.go`, `internal/ingest/pipeline.go`
- Add `FullText string` to the `Source` struct (no interface signature change).
- In `Pipeline.Run`: keep the extracted markdown (currently discarded after `ChunkText`) and set `src.FullText = text`. No new extraction call. `internal/ingest` keeps zero internal imports.

### 3. Store — `internal/db/store.go`
- `upsertSource`: add `full_text` to INSERT + `ON CONFLICT ... DO UPDATE` SET.
- New methods on `SourceStore`:
  - `ListSources(ctx) ([]rag.DocumentInfo, error)` — `SELECT name, type FROM sources ORDER BY name`
  - `GetFullText(ctx, name string) (text string, found bool, err error)` — scan into `*string`; both no-row and NULL full_text return `found=false` (uniform graceful handling).
- Compile-time assertions: `_ rag.DocumentLister = (*SourceStore)(nil)`, `_ rag.FullTextFetcher = (*SourceStore)(nil)`.

### 4. RAG domain — `internal/rag/domain.go`
```go
type DocumentInfo struct{ Name, Type string }
type DocumentLister interface { ListSources(ctx context.Context) ([]DocumentInfo, error) }
type FullTextFetcher interface { GetFullText(ctx context.Context, name string) (string, bool, error) }
type ToolSet struct { Lister DocumentLister; Fetcher FullTextFetcher }
```
Extend `Generator`:
```go
StreamAnswer(ctx, messages []Message, tools ToolSet, onToken, onStatus func(string) error) error
```
`onStatus` may be nil. Only advertise tools whose ToolSet field is non-nil.

### 5. Pipeline — `internal/rag/pipeline.go`
- `Pipeline` gains `lister`/`fetcher` fields; `NewPipeline` gains the two params.
- `Answer` gains `onStatus func(string) error` and passes `ToolSet{lister, fetcher}` to `StreamAnswer`.
- Citations unchanged: derived from initial retrieved chunks only. Known v1 limitation (noted in ADR): a doc the model fetched but that wasn't in top-k won't be cited.

### 6. System prompt — `internal/rag/prompt.go`
Append to `SystemPrompt`: excerpts are primary; if insufficient or incomplete, use `list_documents` then `fetch_full_document` before answering; prefer excerpts when they suffice; never fabricate.

### 7. Tool-use streaming loop — `internal/llm/llm.go` (core change)
Manual loop (do NOT use the beta toolrunner — we need per-token SSE + per-tool status):

- Define `anthropic.ToolParam` for both tools (`fetch_full_document` schema: required string `source_name`); wrap in `[]anthropic.ToolUnionParam{{OfTool: &...}}`.
- Loop (`const maxToolRounds = 5`):
  1. `Messages.NewStreaming` with `Tools`; on round ≥ cap set `ToolChoice: anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}`.
  2. While streaming: `acc.Accumulate(event)` (stitches text + `InputJSONDelta`); forward **only** `TextDelta` to `onToken` (never raw tool-input JSON).
  3. After stream: `params = append(params, acc.ToParam())`.
  4. If `acc.StopReason != anthropic.StopReasonToolUse` → return nil.
  5. Else execute **every** `ToolUseBlock` in `acc.Content` (parallel tool use — a result for each is API-required), collect `anthropic.NewToolResultBlock(tu.ID, text, isErr)`, append as ONE `anthropic.NewUserMessage(results...)`, loop.
- `runTool` dispatch:
  - `list_documents`: `onStatus("Listing available documents…")`; format name+type list.
  - `fetch_full_document`: unmarshal `tu.Input` (json.RawMessage); `onStatus("Reading <name>…")`; `found=false` → non-error tool_result: `No document named %q is available (or it has no stored full text). Use list_documents to see valid names.` — model can recover.
  - Unknown tool → `isError=true`.
- Keep slog logging per round (tool name, round number).

### 8. SSE + server — `internal/server/sse.go`, `internal/server/handlers.go`
- New event: `event: status` / `data: {"msg":"..."}` (add `sseStatus` mirroring `sseToken`). Additive — old clients ignore it.
- `handleChat`: pass `onStatus` callback → `sseStatus`. Status messages are transient — NOT persisted to session history (only `buf` of tokens is, unchanged).
- `server.Answerer` interface updated to the new `Answer` signature.

### 9. Frontend — `internal/server/templates/index.html`
- Add `status` branch in the SSE parse loop → `showStatus(msg)` renders a transient italic line above the streaming bubble; `clearStatus()` on first token, on `finalizeStreamingBubble`, and on `showError`.

### 10. Wiring — `cmd/server/main.go`, `cmd/query/main.go`
- Pass `store` as lister + fetcher into `rag.NewPipeline`.
- `cmd/query`: `onStatus` prints `[Reading resume.pdf…]` to **stderr** (stdout stays clean answer text).
- Keep/update compile-time interface assertions.

### 11. ADR — CREATE `docs/adr/0005-agentic-tool-use-for-full-document-retrieval.md`
Context (chunk-insufficiency failure), decision (tools as escalation, chunks primary), relationship to ADR 0001 (partial reversal: full-doc injection now possible but on-demand and model-driven, not blanket), consequences (5-round cap, re-ingest required, citations remain chunk-derived in v1).

### 12. CONTEXT.md glossary additions
Add terms: **Full Text** (the complete Gemini-extracted markdown of a Source, stored at ingestion), **Tool** (a capability the Resume Agent's model may invoke during answering), **Escalation** (fetching Full Text when retrieved Chunks are insufficient). No implementation details.

## Ops steps (post-merge)

1. Deploy → migration 0003 auto-applies (zero downtime; legacy rows have NULL full_text and degrade gracefully).
2. **Re-ingest every existing source** (`ingest <path-or-url>` via local cloud-sql-proxy, per existing runbook) — `ReplaceSource` upserts on name, populating `full_text`. There is no automatic backfill.
3. Verify: `SELECT name, full_text IS NULL AS missing FROM sources;`

## Test plan (contract, not implementation)

- **`internal/llm/llm_test.go` (new, highest value):** httptest mock of the Anthropic Messages endpoint via `option.WithBaseURL(srv.URL)`, returning SSE-formatted responses. Table-driven: no-tool path; one tool round (assert onToken got final text, onStatus fired, fake fetcher called with right name, 2nd request body contains tool_result); cap-hit (tool_use every round → exactly maxToolRounds+1 requests, last has `tool_choice: none`, final answer still produced); unknown source (graceful tool_result, isError=false, loop continues); parallel tool use (two tool_use blocks → two tool_results in one user turn).
- **`internal/rag/pipeline_test.go`:** update stubGenerator to new signature; assert ToolSet + onStatus threaded through; existing tests updated for new arg.
- **`internal/server/sse_test.go` / `handlers_test.go`:** `status` frame format; fake Answerer emits onStatus → stream contains `status` event before `done`.
- **`internal/db/store_test.go` (integration, skips without TEST_DATABASE_URL):** ListSources; GetFullText three-way (present / NULL / unknown); ReplaceSource round-trips full_text.

## Verification (end-to-end)

1. `go build ./... && go test ./...`
2. `make db-up`, run migration + `ingest` a test PDF, confirm `full_text` populated.
3. `cmd/query "What is anthony's working history?"` — observe `[Reading resume.pdf…]` on stderr and a complete work-history answer.
4. Run `cmd/server` locally, ask the same question in the browser — confirm the transient status indicator appears and the answer is complete.
5. Ask a simple question answerable from chunks — confirm NO tool round occurs (single API call, no status event).

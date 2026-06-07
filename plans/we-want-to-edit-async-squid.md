# Plan: Add `description` to sources, surfaced via `list_documents`

## Context

**Why this change:** When the agent calls the `list_documents` tool it currently only sees
`name (type)` per source — not enough signal to confidently decide *which* document to pull
via `fetch_full_document`. Two similarly-named sources (e.g. two resumes) are
indistinguishable. We want each source to carry a short, LLM-generated **description** of its
contents, surfaced in `list_documents`, so the agent routes to the right document.

**Approach:** During ingest (where the full extracted text is already in hand), call a cheap
model — **`gemini-3.1-flash-lite`** (stable id, verified; the `-preview` variant retires
2026-07-09) — to produce a 1–2 sentence summary. Store it in a new nullable
`sources.description` column and include it in the `list_documents` output. This reuses the
Gemini client already wired into the ingest path (no new API key, no Anthropic client in
ingest), at the cost of slightly stretching ADR 0003's "Gemini = embeddings/extraction only"
boundary — which we record in a new ADR.

**Decisions locked during grilling:**
- Model: `gemini-3.1-flash-lite` (cheapest; Gemini already in ingest path).
- Description shape: **1–2 sentence summary** (~30–50 words).
- Input scope: **full extracted text, capped** at a safe limit (~12k chars/runes) to bound
  cost/latency on large URLs.
- Failure mode: **fatal** — if the describe call fails, the whole ingest fails (no source row
  is written). New ingests are therefore guaranteed to have a description.
- Backfill: **none** — column is nullable; existing/legacy rows stay NULL until re-ingested.
- `list_documents` format: `name (type): description`, falling back to `name (type)` when
  description is NULL/empty.
- Docs: add ADR **0007** + a "Description" glossary term; reconcile the
  `gemini-3.5-flash` vs `gemini-2.0-flash` doc discrepancy.

## ⚠️ Prerequisite — sync the worktree first

This worktree is **2 commits behind `origin/main`**, which already shipped
`internal/db/migrations/0004_add_contact_emails.sql` and
`docs/adr/0006-aws-ses-for-contact-email.md`. **Before writing any code, rebase/merge
`origin/main` into this branch.** Only then are the correct next numbers:
- next migration → **`0005_add_description.sql`**
- next ADR → **`0007-...md`**

If the worktree is *not* synced first, this numbering will collide. Re-confirm the highest
existing migration/ADR number after syncing.

## Changes

### 1. Migration — `internal/db/migrations/0005_add_description.sql`
Mirror the `0003_add_full_text.sql` pattern exactly:
```sql
-- +goose Up
ALTER TABLE sources ADD COLUMN description TEXT;

-- +goose Down
ALTER TABLE sources DROP COLUMN description;
```
Nullable (legacy rows have no description). Embedded via the existing `go:embed` in
`internal/db/migrate.go` — no other wiring needed.

### 2. Domain struct — `internal/ingest/domain.go`
Add a field to `Source`:
```go
Description string // LLM-generated 1–2 sentence summary; empty for legacy rows
```

### 3. Describer port — `internal/ingest/pipeline.go`
Add a consumer-side interface alongside `Embedder`/`PDFExtractor` (per "accept interfaces"):
```go
// Describer generates a short natural-language summary of a source's content,
// used to help the agent choose documents in list_documents.
type Describer interface {
	Describe(ctx context.Context, text string) (string, error)
}
```
- Add a `describer Describer` field to `Pipeline`.
- Add the param to `NewPipeline` (e.g. after `embedder`):
  `NewPipeline(pdfExtractor PDFExtractor, embedder Embedder, describer Describer, urlExtractor URLExtractor, store SourceRepository, log *slog.Logger)`.
- In `Run`, after `extractText` returns `text` and before/after chunking, call it; **fatal on
  error**, then thread into the `Source`:
  ```go
  description, err := p.describer.Describe(ctx, text)
  if err != nil {
      return fmt.Errorf("describe source %s: %w", name, err)
  }
  // ...
  src := Source{Name: name, Type: srcType, Location: location, FullText: text, Description: description}
  ```

### 4. Gemini implementation — new `internal/gemini/describe.go`
Mirror `extract.go`'s structure (package-level const + `GenerateContent`):
```go
const descriptionModel = "gemini-3.1-flash-lite"
const descriptionMaxInputChars = 12000 // cap input to bound cost/latency
const descriptionPrompt = "Write a 1-2 sentence description (max ~40 words) of the " +
	"following document for a knowledge-base index. State what the document is and the key " +
	"topics it covers, so an assistant can decide whether to retrieve it. " +
	"Output only the description, with no preamble or quotes.\n\n"
```
`func (c *Client) Describe(ctx, text) (string, error)`:
- Truncate `text` to `descriptionMaxInputChars` **on a rune boundary** (don't byte-slice a
  multibyte rune — use `utf8`-aware truncation or a small helper).
- `c.inner.Models.GenerateContent(ctx, descriptionModel, <prompt+input as a single user text part>, nil)`.
- Wrap errors with `%w`; return `strings.TrimSpace(resp.Text())`.
- `geminiClient` now also satisfies `ingest.Describer` (verified by being passed to
  `NewPipeline`; no separate assertion needed, matching the existing extractor/embedder style).

### 5. Persist — `internal/db/store.go` (`upsertSource`)
Treat empty as NULL, exactly like `full_text`:
```go
var description *string
if src.Description != "" {
	description = &src.Description
}
```
Add `description` to the INSERT column list/values and to the `ON CONFLICT ... DO UPDATE SET`
clause.

### 6. Read path — `DocumentInfo` + `ListSources`
- `internal/rag/domain.go`: add `Description string` to `DocumentInfo` (additive — existing
  `rag` test stubs still compile).
- `internal/db/store.go` `ListSources`: query
  `SELECT name, type, description FROM sources ORDER BY name`; description is nullable, so scan
  into a `*string` and copy into `d.Description` only when non-nil.

### 7. Tool output — `internal/llm/llm.go`
- In `runTool`, `case toolListDocuments`, format each line as
  `name (type): description`, falling back to `name (type)` when `d.Description == ""`.
- Update the `list_documents` tool `Description` string to mention it now returns a short
  description per document (helps the model use the new signal).

### 8. Wiring — `cmd/ingest/main.go`
Pass `geminiClient` as the describer too (it implements all three Gemini ports):
```go
pipeline := ingest.NewPipeline(geminiClient, geminiClient, geminiClient, ingest.DefaultURLExtractor{}, store, log)
```

### 9. Docs
- **ADR `docs/adr/0007-gemini-flash-lite-for-source-descriptions.md`** — record using
  `gemini-3.1-flash-lite` (a *generation* task) in the ingest path, amending ADR 0003's
  Gemini-only-for-embeddings/extraction boundary; note the fatal-on-failure + nullable-column
  trade-offs.
- **`CONTEXT.md`** — add a "Description" glossary term (a short LLM-generated summary of a
  Source's contents, surfaced in `list_documents`; nullable, legacy rows NULL). Glossary only —
  no implementation details.
- **Reconcile the model-name discrepancy:** code uses `gemini-3.5-flash` for extraction, but
  CONTEXT.md / ADR 0003 say `gemini-2.0-flash`. Update the docs to match the code (or confirm
  with the owner which is intended) while editing them.

## Critical files
- `internal/db/migrations/0005_add_description.sql` (new)
- `internal/gemini/describe.go` (new)
- `internal/ingest/domain.go`, `internal/ingest/pipeline.go`
- `internal/db/store.go`
- `internal/rag/domain.go`
- `internal/llm/llm.go`
- `cmd/ingest/main.go`
- `docs/adr/0007-...md` (new), `CONTEXT.md`, `docs/adr/0003-dual-provider-anthropic-gemini.md`

## Verification
1. **Build/vet/diagnostics:** `go build ./...`, `go vet ./...`, `gofmt -l .`, and check LSP
   diagnostics on every edited file (fix type/import errors immediately).
2. **Migration:** bring up local DB (`make db-up`), run `ingest migrate`, confirm the
   `description` column exists (`\d sources`).
3. **Ingest e2e:** `ingest <sample.pdf>` and `ingest <some-url>`; verify each call succeeds and
   `SELECT name, description FROM sources;` shows a populated 1–2 sentence description.
4. **Failure path (fatal):** temporarily point `GEMINI_API_KEY` at a bad key (or simulate) and
   confirm ingest aborts with a wrapped `describe source ...` error and writes **no** source row.
5. **Tool output:** run the query CLI (`cmd/query`) or server, ask a question that triggers
   `list_documents`, and confirm the output lines read `name (type): description` (and that a
   legacy NULL-description row falls back to `name (type)`).
6. **Tests:** add a unit test for `ListSources` mapping a NULL vs non-NULL `description`, and a
   table test for the `runTool` `list_documents` formatting (with and without description).
   Test the contract (output shape), not the model call.

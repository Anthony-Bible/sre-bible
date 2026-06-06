# Phase 1: Data Foundation — sre.bible Resume Agent

## Context

sre.bible is a RAG-powered resume agent (Go + Cloud SQL/pgvector + Claude generation + Gemini embeddings) documented in `CONTEXT.md`, `docs/adr/0001–0003`, and `plans/implementation-roadmap.md`. The repo is greenfield — docs only, no Go code. Phase 1 gets data flowing: database schema, an `ingest` CLI (PDF/URL → chunks → embeddings → Postgres), verified end-to-end before any UI exists.

**Decisions resolved during grilling (these amend the docs — see Doc Updates):**
- **Embedding model:** `gemini-embedding-2` at **768 dims** (via `output_dimensionality`; auto-normalizes truncated dims). The documented `text-embedding-002` does not exist — docs must be corrected.
- **Database:** local-first. `docker-compose` with `pgvector/pgvector:pg17` for all Phase 1 dev + verification. Cloud SQL provisioning moves to Phase 4. Schema written to be Cloud SQL-compatible.
- **Chunking:** ~1000 chars target, 200-char overlap, paragraph/boundary-aware (never split mid-word). CONTEXT.md's "fixed-size" sharpens to "bounded-size".
- **Migrations:** `pressly/goose` with SQL files embedded via `go:embed`, applied programmatically.
- **Source identity / re-ingest:** identity = citation name (full URL for web, file basename for PDFs). Re-ingest = transactional replace: delete old chunks, insert fresh.
- **Testing:** unit tests for chunker + name-derivation contracts; integration tests for the store layer against local Docker Postgres; embedding client mocked behind an interface.

- **PDF text extraction:** via Gemini document understanding — send the PDF to a Gemini generation model (`gemini-flash`, latest alias) with an "extract as clean markdown" prompt, then chunk → embed the result. Chosen over local Go PDF libraries (janky layout handling) after confirming direct multimodal PDF *embedding* is unsuitable: `gemini-embedding-2` returns one aggregated vector per PDF (max 6 pages) and no extracted text, which breaks chunk-level retrieval and prompt construction.

**Library picks:** `pgx/v5` + `pgvector/pgvector-go`, `codeberg.org/readeck/go-readability` v2 (URL text — the maintained fork of go-shiori/go-readability, per Owner), `google.golang.org/genai` (Gemini SDK — used for both embeddings and PDF text extraction), `pressly/goose/v3`, `log/slog` everywhere (mandatory per user rules).

Go module: `github.com/Anthony-Bible/sre-bible` (matches git remote). Go 1.26.

## File Layout

```
go.mod
docker-compose.yml          # pgvector/pgvector:pg17, port 5432, named volume
Makefile                    # db-up, db-down, migrate, test, ingest helpers
cmd/ingest/main.go          # CLI entrypoint
internal/db/
  db.go                     # pgx pool from DATABASE_URL, pgvector type registration
  migrate.go                # goose embedded migrations (go:embed migrations/*.sql)
  migrations/0001_initial.sql
  store.go                  # SourceStore: ReplaceSource(tx: upsert source, delete+insert chunks)
  store_test.go             # integration tests (skip if DATABASE_URL unset / TEST_DATABASE_URL)
internal/gemini/
  gemini.go                 # one genai client wrapper
  embed.go                  # EmbedDocuments: model gemini-embedding-2, output_dimensionality=768
  extract.go                # ExtractPDFText: gemini-flash + PDF file part → clean markdown
internal/ingest/
  parse.go                  # PDF (via gemini extractor interface) + URL (readeck/go-readability v2) → plain text; name derivation
  chunk.go                  # bounded-size paragraph-aware chunker
  chunk_test.go             # contract tests
  parse_test.go             # name-derivation contract tests
  pipeline.go               # extract/parse → chunk → embed (batched) → store.ReplaceSource
                            # Embedder + PDFExtractor defined as small interfaces here, mocked in tests
```

## Schema (`0001_initial.sql`)

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE sources (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,          -- citation name: URL or file basename
  type        TEXT NOT NULL CHECK (type IN ('pdf','url')),
  location    TEXT NOT NULL,                 -- original path/URL as given
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chunks (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id   BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  idx         INT NOT NULL,                  -- position within source
  content     TEXT NOT NULL,
  embedding   VECTOR(768) NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, idx)
);
CREATE INDEX chunks_embedding_idx ON chunks
  USING hnsw (embedding vector_cosine_ops);

CREATE TABLE sessions (
  id          UUID PRIMARY KEY,              -- generated client/server-side at first page load
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE messages (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  role        TEXT NOT NULL CHECK (role IN ('user','assistant')),
  content     TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_session_idx ON messages (session_id, created_at);
```

(`sessions`/`messages` are created now per the roadmap but unused until Phase 3.)

## CLI Behavior

```
ingest <path-or-url>     # ingest one source (auto-detect: http(s):// → url, else pdf path)
ingest migrate           # apply embedded goose migrations only
```
- Env: `DATABASE_URL` (required), `GEMINI_API_KEY` (required for ingest — used for both PDF extraction and embeddings).
- Migrations auto-applied before ingesting (goose is idempotent).
- Pipeline: extract text (Gemini for PDFs, go-readability for URLs) → chunk → embed in batches (genai batch embed, `RETRIEVAL_DOCUMENT`-style usage per gemini-embedding-2 docs — confirm exact SDK surface at implementation) → single transaction: upsert `sources` row by name, `DELETE FROM chunks WHERE source_id=...`, bulk-insert new chunks.
- `slog` structured logging: source name, chunk count, embedding batch sizes, duration.

## Chunker Contract (drives the unit tests)

- Output chunks ≤ ~1200 chars (hard cap), target ~1000.
- Consecutive chunks share ~200 chars of overlap.
- Prefer splitting at paragraph (`\n\n`) boundaries, then sentence/newline, then word; never mid-word.
- No empty chunks; whitespace-normalized input; full text coverage (every non-whitespace region appears in ≥1 chunk).

## Doc Updates (deferred from grill-with-docs session)

1. **CONTEXT.md** — *Embedding*: replace `text-embedding-002` with `gemini-embedding-2` (768 dimensions). *Chunk*: "fixed-size" → "bounded-size (~1000 characters, paragraph-aware, with overlap)". *Source*: add identity sentence — "A Source is identified by its citation name: the full URL for web sources, the file basename for PDFs. Re-ingesting a Source replaces its Chunks." *Ingestion*: note that PDF parsing is performed by a Gemini generation model (document understanding), not a local PDF library.
2. **ADR 0003** — correct model name to `gemini-embedding-2`, note 768-dim truncation + auto-normalization.
3. **CONTEXT.md** — *Resume Agent*: fix "Gemini for embeddings" model reference if named there.
4. **plans/implementation-roadmap.md** — Phase 1 item 1 becomes "Local Postgres + pgvector via docker-compose"; add "Provision Cloud SQL instance (pgvector enabled)" to Phase 4; correct model name in Phase 1 item 3; move the Gemini client (`internal/embed` → `internal/gemini`, embeddings + PDF extraction) from Phase 2 into Phase 1 (the ingest CLI needs it) — Phase 2 reuses it for query-time embedding and adds `internal/llm` + `internal/rag` (confirmed with Owner).

No new ADR needed (local-first dev is easily reversible and unsurprising).

## Implementation Order

1. `go mod init github.com/Anthony-Bible/sre-bible`; `docker-compose.yml`; `Makefile`
2. Migrations + `internal/db` (pool, goose, store) — integration tests green against Docker
3. `internal/ingest` chunker + parsers — unit tests green (TDD: contract tests first)
4. `internal/gemini` client (embeddings + PDF extraction) behind small interfaces
5. `cmd/ingest` wiring + pipeline
6. Doc updates (item list above)

## Verification

1. `make db-up` → `docker compose` Postgres healthy.
2. `go test ./...` — chunker/parse unit tests + store integration tests pass.
3. `GEMINI_API_KEY=... DATABASE_URL=... go run ./cmd/ingest <some resume PDF>` — logs show chunks embedded + stored.
4. psql smoke test: `SELECT name, count(*) FROM sources JOIN chunks ON chunks.source_id=sources.id GROUP BY name;` and a live cosine query: `SELECT idx, left(content,60), embedding <=> (SELECT embedding FROM chunks LIMIT 1) AS dist FROM chunks ORDER BY dist LIMIT 5;`
5. Re-ingest the same PDF → chunk count stable, no duplicate source rows (replace-by-name verified).

---

# Hexagonal Architecture Refactor

## Context

Phase 1 shipped with a dependency violation: `internal/ingest/pipeline.go` imports `internal/db` for `*db.SourceStore`, `db.Source`, and `db.Chunk`. This inverts the correct direction — the domain core (`ingest`) must never depend on an infrastructure adapter (`db`). The fix moves domain types and the storage port into `ingest`, updates the `db` adapter to import those types and implement the port, and updates the integration tests accordingly. `cmd/ingest/main.go` requires no changes.

**Wrong:** `ingest → db`  
**Correct:** `db → ingest` (adapter depends on domain), `ingest` has zero internal imports

## Files

### 1. NEW `internal/ingest/domain.go`

```go
package ingest

import "context"

// Source is a document or URL in the knowledge base.
// ID is absent — it is a database implementation detail, invisible to the domain.
type Source struct {
    Name     string
    Type     string
    Location string
}

// Chunk is a contiguous text segment with its vector embedding.
// Callers assign sequential Idx values starting at 0.
type Chunk struct {
    Idx       int
    Content   string
    Embedding []float32
}

// SourceRepository is the storage port consumed by Pipeline.
// Interface lives in the consumer package per the "accept interfaces" guideline.
type SourceRepository interface {
    ReplaceSource(ctx context.Context, src Source, chunks []Chunk) error
}
```

### 2. EDIT `internal/ingest/pipeline.go`

- Remove `import "github.com/Anthony-Bible/sre-bible/internal/db"` entirely
- `store *db.SourceStore` → `store SourceRepository`
- `NewPipeline(..., store *db.SourceStore, ...)` → `store SourceRepository`
- `make([]db.Chunk, ...)` → `make([]Chunk, ...)`
- `db.Chunk{...}` → `Chunk{...}`
- `db.Source{...}` → `Source{...}`

### 3. EDIT `internal/db/store.go`

- Add `import "github.com/Anthony-Bible/sre-bible/internal/ingest"`
- Delete exported `Source` struct (lines 16–21)
- Delete exported `Chunk` struct (lines 23–30)
- `ReplaceSource(ctx, src Source, chunks []Chunk)` → `src ingest.Source, chunks []ingest.Chunk`
- `upsertSource(ctx, tx, src Source)` → `src ingest.Source` (body unchanged — same field names `Name`, `Type`, `Location`)
- `insertChunks(ctx, tx, sourceID, chunks []Chunk)` → `chunks []ingest.Chunk` (body unchanged — `c.Idx`, `c.Content`, `c.Embedding` identical)
- Add compile-time assertion after struct declaration: `var _ ingest.SourceRepository = (*SourceStore)(nil)`

### 4. EDIT `internal/db/store_test.go`

- Add `"github.com/Anthony-Bible/sre-bible/internal/ingest"` to imports (`db` import stays for `db.NewSourceStore`, `db.NewPool`, `db.Migrate`)
- Replace all `db.Source{...}` → `ingest.Source{...}` (~10 occurrences)
- Replace all `db.Chunk{...}`, `[]db.Chunk{...}` → `ingest.Chunk{...}`, `[]ingest.Chunk{...}` (~20 occurrences)

### 5. `cmd/ingest/main.go` — no changes

`*db.SourceStore` satisfies `ingest.SourceRepository` implicitly once its `ReplaceSource` signature matches the interface. The composition root can freely hold a concrete type; Go resolves the interface at the call site.

## Verification

```bash
go build ./...                                        # catches import cycles + type mismatches
go vet ./...
go test ./internal/ingest/...                         # unit tests, no DB
TEST_DATABASE_URL=<dsn> go test ./internal/db/...     # integration tests
go list -f '{{.ImportPath}} → {{.Imports}}' ./internal/...
# confirm: internal/ingest has no internal/ imports
# confirm: internal/db imports internal/ingest only
```

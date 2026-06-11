# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Start local Postgres (pgvector/pg17 via Docker/Podman)
make db-up

# Run all migrations
make migrate

# Run all tests (requires DB)
TEST_DATABASE_URL=postgres://sre:sre@localhost:5432/sre_bible?sslmode=disable go test ./... -v -count=1

# Unit tests only (no DB)
make test-unit

# Integration tests (DB-dependent packages)
make test-integration

# Lint
make lint        # go vet ./...

# Run the HTTP server locally (port 8080)
make serve

# Ingest a source (PDF path or URL)
make ingest SRC=path/to/resume.pdf
make ingest SRC=https://example.com/about

# Run a single query against the RAG pipeline
make query Q="What was Anthony's biggest reliability win?"

# Build server binary
make build-server
```

### Required environment variables

| Variable | Required by | Purpose |
|---|---|---|
| `DATABASE_URL` | server, ingest, query | Postgres connection string |
| `GEMINI_API_KEY` | server, ingest, query | Embeddings + PDF extraction |
| `ANTHROPIC_API_KEY` | server, query | Claude generation |
| `TURNSTILE_SITE_KEY` | server | Cloudflare Turnstile (fatal if missing) |
| `TURNSTILE_SECRET_KEY` | server | Cloudflare Turnstile (fatal if missing) |
| `MODEL_ARMOR_TEMPLATE` | server | Model Armor prompt-injection gate template resource name (fatal if missing). Auth via ADC, not an API key. |
| `LISTEN_ADDR` | server | Default `:8080` |
| `CLAUDE_MODEL` | server | Default `claude-haiku-4-5-20251001` |
| `LOG_FORMAT` | server | `json` for structured; default text |
| `EMAIL_FROM`, `EMAIL_TO`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | server | Optional — enables `send_contact_email` tool |
| `METRICS_LISTEN_ADDR` | server | Prometheus scrape listener. Default `:9090` (separate from the public chat port). |
| `OTEL_SERVICE_NAME` | server | Resource attribute attached to every metric. Default `sre-bible`. |

## Architecture

This is a RAG-based conversational agent (`sre.bible`) that answers recruiter/hiring-manager questions about Anthony Bible's professional background, grounded exclusively in ingested source documents.

### Three binaries (`cmd/`)

- **`cmd/server`** — HTTP server serving the chat UI. Runs migrations at startup.
- **`cmd/ingest`** — CLI to ingest a PDF or URL into the knowledge base. Also has `migrate` subcommand.
- **`cmd/query`** — CLI to run a single question through the full RAG pipeline (for testing).

### Ingestion pipeline (`internal/ingest/`)

`ingest.Pipeline.Run(location)` orchestrates: extract text → chunk → (embed + describe concurrently) → store.

- **PDF extraction**: Gemini `gemini-3.5-flash` (multimodal generation), not a local PDF library.
- **URL extraction**: `go-readability` for main content extraction.
- **Chunking**: `ChunkText` in `chunk.go` — ~1000 chars, paragraph-aware, with overlap.
- **Embeddings**: Gemini `gemini-embedding-2`, 768-dim, `RETRIEVAL_DOCUMENT` task type.
- **Source description**: Gemini flash-lite generates a 1–2 sentence summary used by the `list_documents` tool.
- **Storage**: `db.SourceStore.ReplaceSource` — atomic upsert + delete-old-chunks + batch insert via `COPY`.

### RAG query pipeline (`internal/rag/`)

`rag.Pipeline.Answer(sessionID, history, question, onToken, onStatus)`:
1. **Prompt gate (Model Armor)**: screen the inbound question for jailbreak / prompt-injection via `SanitizeUserPrompt` *before* embedding. A match returns the sentinel `rag.ErrPromptBlocked` (handler maps it to a friendly refusal); a Model Armor API error is **fail-open** (allow + log loudly). Skipped when no sanitizer is configured (`cmd/query`, tests). See ADR 0011.
2. Embed question with Gemini (`RETRIEVAL_QUERY` task type — distinct from `RETRIEVAL_DOCUMENT`).
3. Cosine-similarity search via pgvector (`<=>` operator), top-k=8 chunks.
4. Assemble messages (history + enriched current turn with chunk context).
5. Stream via `llm.Client.StreamAnswer` — Anthropic Claude with agentic tool loop (cap: 5 rounds).
6. Return deduplicated citation source names (chunks + tool-fetched documents).

### Agentic tool loop (`internal/llm/`)

Three tools the model may invoke:
- `list_documents` → `SourceStore.ListSources` — returns all sources with names, types, descriptions.
- `fetch_full_document` → `SourceStore.GetFullText` — returns full extracted text of a named source.
- `send_contact_email` → `email.BoundSender.SendContactEmail` — delivers a Viewer-drafted message via AWS SES. Only active when all email env vars are set. Requires `confirmed_draft=true`.

### HTTP server (`internal/server/`)

- `GET /` — renders `templates/index.html` with suggested questions and Turnstile site key.
- `GET /messages` — returns session message history as JSON; session ID from cookie.
- `POST /chat` — Cloudflare Turnstile gate (on first request per session), then runs RAG pipeline, streams response as SSE, persists both turns.
- `GET /healthz` / `GET /readyz` — liveness (always 200) / readiness (DB ping).

SSE events: `token` (text delta), `status` (transient tool-use message), `done` (citations JSON), `error`.

Turnstile: checked once per session. Session marked `verified` in DB after first successful check. `TURNSTILE_SITE_KEY` and `TURNSTILE_SECRET_KEY` are both required at startup.

### Database (`internal/db/`)

- PostgreSQL 17 + pgvector extension.
- Migrations via Goose (`pressly/goose/v3`), embedded SQL in `internal/db/migrations/`. Run automatically at server startup.
- **Migrations must be idempotent.** Use `IF NOT EXISTS` / `IF EXISTS` guards on all `ADD COLUMN`, `DROP COLUMN`, `CREATE TABLE`, `CREATE INDEX`, etc. so re-running a migration against a DB that already has the change is safe.
- Connection pool: max 5 conns (sized for Cloud SQL `db-f1-micro`).
- `SourceStore` satisfies `ingest.SourceRepository`, `rag.ChunkSearcher`, `rag.DocumentLister`, `rag.FullTextFetcher` — compile-time assertions in `cmd/server/main.go`.
- `SessionStore` satisfies `server.SessionRepository`.

### Interface design

Interfaces are defined at the **consumption** site (not where they are implemented). Compile-time assertions (`var _ Interface = (*Impl)(nil)`) live in `cmd/server/main.go` to avoid import cycles between `db`, `rag`, and `server`.

### Email (`internal/email/`)

AWS SES via `aws-sdk-go-v2`. Rate-limited: at most one email per session, plus a global hourly cap (default 24, overridable via `EMAIL_RATE_LIMIT_PER_HOUR`). `BoundSender` carries a session ID for the per-session enforcement.

### Metrics (`internal/metrics/`)

OpenTelemetry metrics exported to Prometheus. The server runs a second HTTP listener (`METRICS_LISTEN_ADDR`, default `:9090`) that exposes `/metrics`. Every instrument lives on the package-level singleton `metrics.M` — before `metrics.Init()` runs, `M` is backed by a no-op provider so CLI binaries (`cmd/ingest`, `cmd/query`, tests) work without configuration.

**Adding a new metric:**

1. Add a field to the `Metrics` struct in `internal/metrics/metrics.go` (`Int64Counter`, `Float64Histogram`, `Int64UpDownCounter`, etc.).
2. Initialise it inside `newMetrics()` with `meter.Int64Counter("sre_bible_<name>", metric.WithDescription(...))`. Keep the `sre_bible_` prefix.
3. At the call site, call `metrics.M.YourField.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("key","value")))` or `.Record(ctx, value, ...)`. No constructor wiring needed.
4. Keep attribute cardinality bounded — never use raw user input as a label value. Use enum-like outcomes (`ok`, `error`, `not_found`, etc.).

Current instruments cover HTTP traffic (requests, duration, in-flight), sessions, LLM outcomes (served, blocked by reason, errors by stage, duration), per-tool calls, RAG retrieval, ingestion stages, and Turnstile checks.

## Key architectural decisions (see `docs/adr/`)

- **RAG chosen over direct context injection** — portfolio demonstration and extensibility, despite resume content fitting in Claude's 200K context window.
- **Anthropic (generation) + Gemini (embeddings + PDF extraction)** — Claude for grounding quality, Gemini embedding-2 for cost-effective 768-dim vectors. Switching the embedding model requires re-ingesting all sources.
- **Cloud SQL + pgvector** — managed Postgres on GCP, pgvector for cosine similarity; avoids a separate vector DB service.
- **Cloudflare Turnstile** gates `POST /chat` to prevent automated abuse. Both site key and secret key are required at startup (not optional).

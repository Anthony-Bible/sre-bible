# Implementation Roadmap — sre.bible Resume Agent

## Phase 1: Data Foundation ✅
**Goal:** Get data flowing before building any UI.

1. Local Postgres + pgvector via `docker-compose` (`pgvector/pgvector:pg17`); Cloud SQL provisioning moved to Phase 4
2. Database schema: `sources`, `chunks`, `sessions`, `messages` tables (goose migrations, SQL embedded in binary)
3. `cmd/ingest` CLI binary — accepts a PDF path or URL, extracts text (Gemini `gemini-2.0-flash` for PDFs, go-readability for URLs), chunks (~1000 chars, paragraph-aware, 200-char overlap), calls Gemini `gemini-embedding-2` (768 dims), writes to local Postgres via `internal/db` + `internal/gemini`
4. Verify end-to-end: ingest a resume PDF, query vectors directly in psql

## Phase 2: Core RAG + LLM
**Goal:** Answer questions from the command line before touching the web layer.

1. `internal/db` — add pgvector similarity search query (pool + store already exist from Phase 1)
2. `internal/gemini` — add query-time embedding (client already exists from Phase 1; reuse for embed-at-query)
3. `internal/llm` — Anthropic Claude client (streaming)
4. `internal/rag` — retrieval pipeline: query → embed → similarity search → retrieve chunks → build prompt → stream response
5. Verify end-to-end: ask a question via a test harness, get a grounded answer back

## Phase 3: Web Server
**Goal:** Chat interface at sre.bible.

1. `cmd/server` — Go HTTP server
2. Go HTML templates + HTMX for chat UI
3. SSE endpoint for streaming Claude responses to the browser
4. Session management (anonymous UUID, persisted to Cloud SQL)
5. Suggested questions (3-4 hardcoded prompts)
6. Markdown rendering via `marked.js`
7. Footnote-style source citations at bottom of each response

## Phase 4: Polish + Deploy
**Goal:** Production-ready on GKE.

1. Provision Cloud SQL instance with pgvector enabled; re-ingest all Sources against Cloud SQL
2. Cloudflare DNS + proxying for `sre.bible` (rate limiting configured at Cloudflare level)
3. Kubernetes manifests (Deployment, Service, ConfigMap, Secret for API keys)
4. Cloud SQL Auth Proxy sidecar
5. Header: name, title, one-liner bio
6. Final suggested questions tuned to strongest talking points

## Key Architectural Decisions
See `docs/adr/` for rationale on:
- RAG over direct context injection (`0001`)
- Cloud SQL + pgvector as the single data store (`0002`)
- Anthropic (generation) + Gemini (embeddings) dual-provider (`0003`)

## Domain Language
See `CONTEXT.md` for canonical definitions of: Viewer, Session, Source, Chunk, Embedding, Retrieval, Ingestion, Owner, Resume Agent, Suggested Questions, Citation.

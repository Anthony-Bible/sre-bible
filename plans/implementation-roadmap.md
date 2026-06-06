# Implementation Roadmap — sre.bible Resume Agent

## Phase 1: Data Foundation
**Goal:** Get data flowing before building any UI.

1. Cloud SQL instance provisioned with pgvector extension enabled
2. Database schema: `sources`, `chunks`, `sessions`, `messages` tables
3. `cmd/ingest` CLI binary — accepts a PDF path or URL, chunks it, calls Gemini `text-embedding-002`, writes to Cloud SQL
4. Verify end-to-end: ingest a resume PDF and query vectors directly in psql

## Phase 2: Core RAG + LLM
**Goal:** Answer questions from the command line before touching the web layer.

1. `internal/db` — Cloud SQL connection, pgvector similarity search
2. `internal/embed` — Gemini embedding client
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

1. Cloudflare DNS + proxying for `sre.bible` (rate limiting configured at Cloudflare level)
2. Kubernetes manifests (Deployment, Service, ConfigMap, Secret for API keys)
3. Cloud SQL Auth Proxy sidecar
4. Header: name, title, one-liner bio
5. Final suggested questions tuned to strongest talking points

## Key Architectural Decisions
See `docs/adr/` for rationale on:
- RAG over direct context injection (`0001`)
- Cloud SQL + pgvector as the single data store (`0002`)
- Anthropic (generation) + Gemini (embeddings) dual-provider (`0003`)

## Domain Language
See `CONTEXT.md` for canonical definitions of: Viewer, Session, Source, Chunk, Embedding, Retrieval, Ingestion, Owner, Resume Agent, Suggested Questions, Citation.

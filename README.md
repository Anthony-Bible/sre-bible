# sre.bible

A conversational AI agent that answers questions about [Anthony Bible's](https://anthonybible.com) professional background. Visitors ask questions in natural language; the agent answers from ingested source documents (PDF resume, personal site, etc.) using RAG, streaming each response token-by-token.

Live at **[sre.bible](https://sre.bible)**.

---

## How it works

### Query path

```mermaid
sequenceDiagram
    participant V as Visitor (Browser)
    participant S as HTTP Server
    participant G as Gemini (embedding)
    participant DB as Cloud SQL / pgvector
    participant C as Anthropic Claude

    V->>S: POST /chat (question + Turnstile token)
    S->>S: Verify Turnstile (first request per session)
    S->>G: EmbedQuery(question) — RETRIEVAL_QUERY
    G-->>S: 768-dim vector
    S->>DB: cosine search top-8 chunks (<=> operator)
    DB-->>S: RetrievedChunks
    S->>C: StreamAnswer(history + chunks as context)
    loop Tool use (≤5 rounds)
        C-->>S: tool_use: list_documents
        S->>DB: ListSources()
        DB-->>S: source names + descriptions
        S->>C: tool_result
        C-->>S: tool_use: fetch_full_document
        S->>DB: GetFullText(name)
        DB-->>S: full source text
        S->>C: tool_result
    end
    C-->>S: token stream
    S-->>V: SSE (token / status / done events)
    S->>DB: AppendMessage (user + assistant turns)
```

### Ingestion path

```mermaid
flowchart LR
    src([PDF or URL]) --> extract

    subgraph ingest ["ingest CLI"]
        extract["Extract text\n(Gemini flash for PDF\ngo-readability for URL)"]
        chunk["Chunk\n~1000 chars, paragraph-aware"]
        parallel((" "))
        embed["Embed chunks\ngemini-embedding-2\n768-dim RETRIEVAL_DOCUMENT"]
        describe["Describe source\nGemini flash-lite\n1–2 sentence summary"]
        store["ReplaceSource\nupsert + delete old chunks\n+ COPY new chunks"]

        extract --> chunk --> parallel
        parallel --> embed
        parallel --> describe
        embed --> store
        describe --> store
    end

    store --> db[(Cloud SQL\npgvector)]
```

Sources are ingested offline via a CLI: PDF text is extracted by Gemini's multimodal generation (not a local PDF library), chunked at ~1000 chars with overlap, embedded with `gemini-embedding-2` (768-dim), and stored in Cloud SQL with pgvector.

## Stack

| Layer | Technology |
|---|---|
| Generation | Anthropic Claude (`claude-haiku-4-5-20251001` default) |
| Embeddings | Google Gemini `gemini-embedding-2` (768-dim) |
| PDF extraction | Gemini `gemini-3.5-flash` (multimodal) |
| Vector store | Cloud SQL (PostgreSQL 17) + pgvector |
| Migrations | Goose |
| Email delivery | AWS SES v2 |
| Bot protection | Cloudflare Turnstile |
| Runtime | Go 1.26, standard library HTTP |
| Deploy | GKE + ArgoCD + Cloud SQL Auth Proxy |

## Local development

**Prerequisites:** Go 1.26+, Docker or Podman, API keys for Gemini and Anthropic, Cloudflare Turnstile keys.

```bash
# Start Postgres with pgvector
make db-up

# Run the server (serves on :8080)
DATABASE_URL=postgres://sre:sre@localhost:5432/sre_bible?sslmode=disable \
  GEMINI_API_KEY=<key> \
  ANTHROPIC_API_KEY=<key> \
  TURNSTILE_SITE_KEY=1x00000000000000000000AA \
  TURNSTILE_SECRET_KEY=1x0000000000000000000000000000000AA \
  make serve
```

For local dev, use [Cloudflare's always-pass test keys](https://developers.cloudflare.com/turnstile/troubleshooting/testing/): site key `1x00000000000000000000AA`, secret `1x0000000000000000000000000000000AA`.

## Ingesting sources

```bash
# Ingest a PDF
make ingest SRC=/path/to/resume.pdf

# Ingest a URL
make ingest SRC=https://yoursite.com/about
```

Re-ingesting an existing source replaces it atomically (upsert + chunk replacement in a single transaction).

## Running tests

```bash
make test              # all tests (requires DB running)
make test-unit         # pure unit tests, no DB
make test-integration  # DB-dependent packages only
```

## Environment variables

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | Yes | Postgres connection string |
| `GEMINI_API_KEY` | Yes | Embeddings, PDF extraction, source descriptions |
| `ANTHROPIC_API_KEY` | Yes | Chat generation |
| `TURNSTILE_SITE_KEY` | Yes | Cloudflare Turnstile public key |
| `TURNSTILE_SECRET_KEY` | Yes | Cloudflare Turnstile secret key |
| `CLAUDE_MODEL` | No | Default: `claude-haiku-4-5-20251001` |
| `LISTEN_ADDR` | No | Default: `:8080` |
| `LOG_FORMAT` | No | `json` for structured logging; default is text |
| `EMAIL_FROM` | No | Enables contact email tool when all email vars are set |
| `EMAIL_TO` | No | Destination address for contact emails |
| `AWS_REGION` | No | SES region |
| `AWS_ACCESS_KEY_ID` | No | SES IAM credentials |
| `AWS_SECRET_ACCESS_KEY` | No | SES IAM credentials |
| `EMAIL_RATE_LIMIT_PER_HOUR` | No | Global hourly cap on outbound emails; default 24 |

## Deployment

See [`deploy/README.md`](deploy/README.md) for the full operations runbook: Terraform provisioning, GKE secrets, ArgoCD bootstrap, re-ingestion procedure, and troubleshooting.

The deploy flow after initial setup is:
1. Push to `main` → GitHub Actions builds and pushes the image to GHCR.
2. Update the image digest in `deploy/deployment.yaml`.
3. Push → ArgoCD auto-syncs within ~3 minutes.

## Architecture decisions

Key decisions are recorded in [`docs/adr/`](docs/adr/):

- [ADR 0001](docs/adr/0001-rag-over-direct-context-injection.md) — RAG over direct context injection
- [ADR 0002](docs/adr/0002-cloudsql-pgvector-as-vector-store.md) — Cloud SQL + pgvector as vector store
- [ADR 0003](docs/adr/0003-dual-provider-anthropic-gemini.md) — Anthropic for generation, Gemini for embeddings
- [ADR 0004](docs/adr/0004-deployment-topology.md) — Deployment topology
- [ADR 0005](docs/adr/0005-agentic-tool-use-for-full-document-retrieval.md) — Agentic tool use for full-document retrieval
- [ADR 0006](docs/adr/0006-aws-ses-for-contact-email.md) — AWS SES for contact email
- [ADR 0007](docs/adr/0007-gemini-flash-lite-for-source-descriptions.md) — Gemini Flash Lite for source descriptions

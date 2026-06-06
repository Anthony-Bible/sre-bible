# ADR 0002 — Cloud SQL (pgvector) as Vector Store

## Status
Accepted

## Context
The RAG pipeline requires a vector store for Embeddings. Purpose-built options (Qdrant, Weaviate, Pinecone) and managed GCP options (Vertex AI Vector Search, AlloyDB) were considered alongside pgvector on Cloud SQL.

## Decision
Use Cloud SQL (Postgres + pgvector extension) as the single data store for vectors, session data, source metadata, and conversation history.

## Rationale
At the traffic scale of a personal resume agent, a purpose-built vector database provides no meaningful performance advantage over pgvector. Consolidating into a single Cloud SQL instance:

1. **Reduces operational surface** — one managed database instead of Postgres + a separate vector service.
2. **Simplifies the Go data layer** — one connection pool, one migration tool, one place to look.
3. **GCP-native** — Cloud SQL integrates cleanly with the GKE deployment via Cloud SQL Auth Proxy.
4. **Cost** — one managed instance vs. two services.

Purpose-built vector DBs (Qdrant, Pinecone) would be preferred if retrieval latency at high query volume became a bottleneck — not an anticipated concern for this use case.

## Consequences
- All persistent state (vectors, sessions, sources, conversation history) lives in one Cloud SQL instance.
- pgvector's ANN (approximate nearest neighbor) performance is sufficient for the corpus size.
- Migration to a dedicated vector store later would require an ETL step.

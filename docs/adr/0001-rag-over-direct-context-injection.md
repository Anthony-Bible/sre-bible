# ADR 0001 — RAG over Direct Context Injection

## Status
Accepted

## Context
The Resume Agent needs to answer Viewer questions grounded in Owner-provided Sources (PDFs, URLs). Two architectural approaches were considered:

**Direct Context Injection** — parse all Sources at startup, stuff the full text into every LLM request. Simple, no vector store, works because Claude's context window (200K tokens) easily fits a resume's worth of material.

**RAG (Retrieval-Augmented Generation)** — chunk Sources, embed chunks via Gemini `text-embedding-002`, store in pgvector, retrieve semantically relevant chunks per query, pass only those to the LLM.

## Decision
Use RAG.

## Rationale
The total Source material for a resume agent would fit in Claude's context window, making RAG architecturally unnecessary for correctness or scale. However, RAG was chosen because:

1. **Skill demonstration** — `sre.bible` is itself a portfolio artifact. A visitor who inspects the architecture should see a production-grade RAG pipeline, not a context-stuffing shortcut.
2. **Extensibility** — RAG scales gracefully if the Owner adds many Sources over time. Direct injection degrades as context fills.
3. **Cost profile** — RAG sends only relevant chunks per query rather than the full corpus, reducing token spend at scale.

## Consequences
- Requires Cloud SQL with pgvector, a Gemini embedding client, and a chunking/retrieval layer.
- Adds ingestion pipeline complexity vs. direct injection.
- Chunking strategy and retrieval quality become ongoing tuning concerns.

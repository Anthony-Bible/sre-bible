# ADR 0003 — Dual AI Provider (Anthropic for Generation, Gemini for Embeddings)

## Status
Accepted

## Context
The system requires two distinct AI capabilities: text generation (answering Viewer questions) and text embedding (vectorizing Chunks for RAG). Using a single provider for both was the simpler option.

## Decision
Use Anthropic Claude for generation and Google Gemini `text-embedding-002` for embeddings.

## Rationale
1. **Skill demonstration** — using both providers shows breadth of AI platform experience, which is relevant for a system that is itself a portfolio artifact.
2. **Quality fit** — Anthropic Claude has strong grounding behavior (less hallucination on RAG tasks) and is the preferred generation model. Gemini `text-embedding-002` is a high-quality, cost-effective embedding model with a well-supported API.
3. **Embedding lock-in is acceptable** — once Chunks are embedded with a given model, switching requires re-embedding all Sources. This cost is low for a resume-scale corpus, making the initial choice low-risk.

## Consequences
- Two API credentials to manage (Anthropic API key, Google AI / Vertex AI credentials).
- Two Go client libraries in the dependency tree.
- Switching the generation model (e.g., to GPT-4o) is straightforward — the embedding model is harder to swap without re-ingesting all Sources.

# CONTEXT.md — sre.bible Resume Agent

## Glossary

### Viewer
A person who visits `sre.bible` and interacts with the Resume Agent. Primarily recruiters and hiring managers, secondarily technical peers (engineers, CTOs). Viewers are always anonymous — no login or registration required.

### Session
An anonymous, server-side conversation between a Viewer and the Resume Agent. Each Session is identified by a UUID generated at first page load. Sessions are persisted indefinitely in Cloud SQL and are visible to the Owner for analytics. Sessions contain no PII.

### Source
A document or URL provided by the Owner that forms the knowledge base of the Resume Agent. Supported types at launch: PDF files and web URLs. Sources are ingested via the `ingest` CLI — not through the web interface. A Source is identified by its citation name: the full URL for web sources, the file basename for PDFs. Re-ingesting a Source replaces its Chunks.

### Chunk
A bounded-size segment (~1000 characters, paragraph-aware, with overlap) of a Source produced during ingestion. Each Chunk is embedded independently and stored in Cloud SQL alongside its vector. At query time, the most semantically relevant Chunks are retrieved and passed to the LLM as context. PDF text is extracted via a Gemini generation model (document understanding) before chunking — not by a local PDF library.

### Embedding
A 768-dimensional vector representation of a Chunk's text, produced by the Gemini `gemini-embedding-2` model (output dimensionality truncated to 768 and auto-normalized). Embeddings are stored in Cloud SQL via pgvector and used for semantic similarity search during retrieval.

### Retrieval
The process of finding the Chunks most semantically relevant to a Viewer's query. Retrieval uses cosine similarity search against stored Embeddings in Cloud SQL.

### Ingestion
The full pipeline of processing a Source into queryable knowledge: parsing → chunking → embedding → storing Chunks and Embeddings in Cloud SQL. Triggered manually by the Owner via the `ingest` CLI binary.

### Owner
Anthony Bible. The person who deploys the system, runs ingestion, and has read access to Session data and logs. The Owner is the subject of all Source material.

### Resume Agent
The conversational AI interface at `sre.bible`. Answers Viewer questions about the Owner's professional background, grounded exclusively in ingested Sources. Powered by Anthropic Claude for generation and Gemini for embeddings.

### Suggested Questions
A curated set of 3–4 hardcoded prompts displayed to the Viewer before their first message. Intended to reduce friction and guide Viewers toward the Owner's strongest talking points. Disappear once the conversation begins.

### Citation
A footnote-style source attribution displayed at the bottom of each Resume Agent response, listing which Sources the answer was drawn from (e.g., `resume.pdf`, `anthonybible.com/about`). Citations are source-level, not chunk-level.

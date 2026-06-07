# CONTEXT.md — sre.bible Resume Agent

## Glossary

### Viewer
A person who visits `sre.bible` and interacts with the Resume Agent. Primarily recruiters and hiring managers, secondarily technical peers (engineers, CTOs). Viewers are always anonymous — no login or registration required.

### Session
An anonymous, server-side conversation between a Viewer and the Resume Agent. Each Session is identified by a UUID generated at first page load. Sessions are persisted indefinitely in Cloud SQL and are visible to the Owner for analytics. Sessions contain no system-collected PII; Viewers may volunteer contact details, which appear in Message content and Contact Email records.

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

### Contact Email
A Viewer-initiated message that the Resume Agent delivers to the Owner via email. The Resume Agent composes the message from the conversation, shows the Viewer the draft, and sends only after explicit Viewer approval — at most one per Session, with a global hourly cap. Delivered via AWS SES; the Viewer's email address becomes the Reply-To. Viewer name and email are stored in the `contact_emails` table for audit purposes.

### Suggested Questions
A curated set of 3–5 hardcoded prompts displayed to the Viewer before their first message. Intended to reduce friction and guide Viewers toward the Owner's strongest talking points. Disappear once the conversation begins.

### Citation
A footnote-style source attribution displayed at the bottom of each Resume Agent response, listing which Sources the answer was drawn from (e.g., `resume.pdf`, `anthonybible.com/about`). Citations are source-level, not chunk-level.

### Message
A single conversational turn within a Session. Each Message has a role (`user` or `assistant`) and a text content. User Messages contain the Viewer's question. Assistant Messages contain the Resume Agent's response. Messages are persisted to Cloud SQL and are visible to the Owner for analytics.

### Full Text
The complete Gemini-extracted markdown of a Source, stored in the `sources.full_text` column at ingestion time. Used by the Resume Agent when retrieved Chunks are insufficient to answer a question — see Tool and Escalation. Nullable: legacy rows ingested before this column existed have `full_text = NULL` and degrade gracefully.

### Tool
A capability the Resume Agent's model may invoke during answer generation, beyond the initial chunk context. Three tools exist: `list_documents` (returns all Source names and types), `fetch_full_document` (returns the Full Text of a named Source), and `send_contact_email` (delivers a Viewer-composed message to the Owner via AWS SES — see Contact Email). Tools are defined in `internal/llm` and exposed to the model via the Anthropic tool-use API. The tool loop is capped at 5 rounds.

### Escalation
The act of the Resume Agent fetching a Source's Full Text when the initially retrieved Chunks are insufficient to answer a Viewer's question. Escalation is model-driven — the Resume Agent decides when Chunks are inadequate and calls the appropriate Tool. Escalation produces a transient status message visible in the chat UI ("Reading resume.pdf…") but does not alter session history or citations.

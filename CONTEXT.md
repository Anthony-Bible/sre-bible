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
A footnote-style source attribution displayed at the bottom of each Resume Agent response, listing which Sources the answer was drawn from (e.g., `resume.pdf`, `anthonybible.com/about`). Citations are source-level, not chunk-level. Each Citation is clickable: selecting it reveals the Grounding Excerpts retrieved from that Source (or, for a Source the Resume Agent reached only via Escalation, a "fetched full document" note with no excerpt).

### Message
A single conversational turn within a Session. Each Message has a role (`user` or `assistant`) and a text content. User Messages contain the Viewer's question. Assistant Messages contain the Resume Agent's response. Messages are persisted to Cloud SQL and are visible to the Owner for analytics.

### Description
A short LLM-generated summary (1–2 sentences, ≤ ~40 words) of a Source's contents, stored in the `sources.description` column at ingestion time. Produced by `gemini-3.1-flash-lite` during ingest from the full extracted text. Surfaced by the `list_documents` tool as `name (type): description` so the Resume Agent can route to the right document without always fetching full text. Nullable: legacy rows ingested before this column existed have `description = NULL` and degrade gracefully (the description suffix is omitted in `list_documents` output).

### Full Text
The complete Gemini-extracted markdown of a Source, stored in the `sources.full_text` column at ingestion time. Used by the Resume Agent when retrieved Chunks are insufficient to answer a question — see Tool and Escalation. Nullable: legacy rows ingested before this column existed have `full_text = NULL` and degrade gracefully.

### Tool
A capability the Resume Agent's model may invoke during answer generation, beyond the initial chunk context. Three tools exist: `list_documents` (returns all Source names and types), `fetch_full_document` (returns the Full Text of a named Source), and `send_contact_email` (delivers a Viewer-composed message to the Owner via AWS SES — see Contact Email). Tools are defined in `internal/llm` and exposed to the model via the Anthropic tool-use API. The tool loop is capped at 5 rounds.

### Escalation
The act of the Resume Agent fetching a Source's Full Text when the initially retrieved Chunks are insufficient to answer a Viewer's question. Escalation is model-driven — the Resume Agent decides when Chunks are inadequate and calls the appropriate Tool. Each Escalation is recorded as a `tool_call` Trace Step in the persisted Agent Trace, so it remains visible after reload and to the Owner for analytics.

### Agent Trace
The persisted, ordered record of the steps the Resume Agent took to produce a single assistant Message — retrieval, any Tool calls (Escalations), and the final answer. Stored per Message in the `messages.trace` JSONB column and returned by the message-history API, so it survives reload. Displayed as a collapsible per-Message timeline in the chat UI (auto-expanded while streaming, collapsed to a one-line summary once the answer completes). Recorded with a strict allow-list of safe data — curated step labels plus document names and counts — and never the system prompt, internal prompts, raw tool payloads, or any Contact Email content.

### Trace Step
A single entry in an Agent Trace. Each Step has a kind — `retrieval` (the Chunk search: how many Chunks from how many Sources, plus the Grounding Excerpts), `tool_call` (one Tool invocation: tool name, safe target document name, and outcome), or `answer` (the final composition: number of tool rounds and duration) — and a human-readable label. A `send_contact_email` Tool call is recorded with a generic label and no target, never the message details.

### Grounding Excerpt
The exact text of a retrieved Chunk, paired with its Source name, carried in the `retrieval` Trace Step. Grounding Excerpts are what a Citation reveals when clicked, letting a Viewer see the precise source passage an answer was drawn from. They contain the same Chunk text already sent to the model and are rendered as plain text (never interpreted as markup).

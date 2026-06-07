# ADR 0005 — Agentic Tool Use for Full-Document Retrieval

**Date:** 2026-06-06  
**Status:** Accepted  
**Deciders:** Anthony Bible

---

## Context

The pure-RAG architecture established in ADR 0001 retrieves top-k chunks (k=8) and passes them directly to the LLM for generation. This works well for questions whose answers are concentrated in a small number of chunks, but fails when the answer spans more content than the retrieved excerpts contain.

**Observed failure:** "What is anthony's working history?" → the retrieved chunks contained partial resume sections, but the LLM replied that the information was not available — even though the full document had it. Root cause: Gemini-extracted markdown was discarded after chunking (`internal/ingest/pipeline.go`); there was no full-text column, and `SearchChunks` returned only chunk content.

---

## Decision

Store the complete Gemini-extracted markdown in a new nullable `sources.full_text` column, and expose it to the LLM via two model-callable tools:

- **`list_documents`** — returns all source names and types  
- **`fetch_full_document(source_name)`** — returns the full text of a named source

The model decides when to escalate. Retrieved chunks remain the primary context (top-k=8 unchanged). Tool use is an escalation path, not the default.

### Key constraints

- **Tool loop cap:** 5 rounds. On round 5 the API is called with `tool_choice: none` to force a final answer regardless of what the model wants to do next.
- **Always on:** no config flag. The tools are always advertised when both `DocumentLister` and `FullTextFetcher` are non-nil (which they always are in production).
- **Status SSE event:** a new `event: status` frame (data: `{"msg":"..."}`) is emitted to the browser during tool rounds so the user sees "Reading resume.pdf…" rather than a blank streaming bubble. Status messages are transient — not persisted to session history.
- **Citations remain chunk-derived (v1):** only the initially retrieved chunks produce citations. A document the model fetched via tool that was not in the top-k will not appear in the citation list.
- **Re-ingest required:** existing sources have `full_text = NULL` (graceful degradation — the model will receive a "no stored full text" tool result and can fall back to chunks). A manual re-ingest is required to populate `full_text` for existing sources.

---

## Relationship to ADR 0001

ADR 0001 established a pure-RAG stance: retrieve chunks, inject as context, generate. This ADR partially reverses that stance by allowing on-demand full-document injection — but only when the model explicitly requests it, and only via a capped tool loop. The chunk-first approach is preserved; full-document fetch is an escalation, not a replacement.

---

## Consequences

**Positive:**
- Eliminates the "answer not available" failure for questions that span more than k chunks.
- Model-driven escalation avoids injecting full documents into every prompt (token cost).
- Graceful degradation: legacy sources without `full_text` produce a recoverable tool error; the model can still answer from chunks.

**Negative:**
- Tool rounds add latency (one extra API round-trip per tool call).
- Citations remain chunk-derived in v1; documents fetched via tool are not cited.
- Re-ingest of all existing sources is required to populate `full_text`.
- The 5-round cap is a heuristic; adversarial or confused models could waste all 5 rounds without producing a useful answer.

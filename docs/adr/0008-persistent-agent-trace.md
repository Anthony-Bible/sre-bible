# ADR 0008 — Persistent Agent Trace + Citation Grounding Reveal

**Date:** 2026-06-07  
**Status:** Accepted  
**Deciders:** Anthony Bible

---

## Context

`sre.bible` is both a working RAG agent and a portfolio piece meant to *demonstrate* agentic-engineering skill. Today that skill is largely invisible to a viewer:

- The agent's reasoning surfaced only as a transient `event: status` SSE frame (introduced in ADR 0005), rendered in a single `#status-indicator` that showed the latest string and then **vanished** — and was never persisted (ADR 0005 explicitly called status messages transient).
- The **retrieval** step emitted no signal at all.
- The grounding chunk text assembled in `BuildUserMessage` was **discarded** after the prompt was built and never reached the browser.

The net effect: a recruiter sees streamed prose with no evidence that an agent retrieved, escalated, or grounded anything — and on reload, even the transient hints are gone. There was no after-the-fact record of *why* the agent answered the way it did.

---

## Decision

Promote the ephemeral status signal into a **persistent, structured Agent Trace** stored per assistant message, plus a **source-level grounding reveal** behind each citation.

### Structured trace steps

A single flat `rag.TraceStep` with a `Kind` discriminator (`retrieval` | `tool_call` | `answer`) and optional pointer detail structs (`RetrievalDetail`, `ToolCallDetail`, `AnswerDetail`). This serializes to one ordered JSONB array, preserves step order, and — critically — the typed structs act as a **PII allow-list by construction**: only fields we explicitly model can ever be stored.

- **`retrieval`** — emitted by `rag.Pipeline.Answer` right after chunk search, on **both** the normal and zero-chunk paths. Carries `chunk_count`, `source_count`, and one `GroundingExcerpt{source_name, text}` per retrieved chunk. This is the only place chunk `Content` is captured before `BuildUserMessage` consumes it — no DB schema change to chunks is needed.
- **`tool_call`** — emitted by `internal/llm` once per tool-use block. Carries the tool name, a SAFE `target` (document name only), and an `outcome` (`ok` | `error` | `not_found` | `refused`).
- **`answer`** — emitted by `StreamAnswer` before each non-error return; carries `tool_rounds` and `duration_ms`.

### Replace `onStatus`/`status` with `onTrace`/`trace`

- The `rag.Generator.StreamAnswer` and `rag.Pipeline.Answer` callbacks drop `onStatus func(string)` in favor of `onTrace func(TraceStep) error`.
- The SSE `event: status` frame and the `#status-indicator` element are **removed**; a new `event: trace` frame (`data: {"step":{…}}`) is emitted as each step occurs.
- `handleChat` accumulates the streamed steps into a `[]rag.TraceStep` and persists it with the assistant turn.

### Persistence

A nullable `messages.trace JSONB` column (migration `0008`, idempotent). `NULL` distinguishes "no trace recorded" (legacy rows, user turns) from an empty trace. `AppendMessage` gains a `trace []rag.TraceStep` argument (per-turn data, like `citations`); `ListMessages` returns it via `GET /messages` so the trace **survives reload**. Unmarshal failures log-and-degrade to `nil` rather than failing the list.

### Source-level grounding reveal

Citations become clickable. Clicking a citation reveals the exact retrieved chunk excerpt(s) for that source, grouped from the `retrieval` step's excerpts by `source_name`. A citation produced only by a tool-fetched document (not in the top-k) has no excerpt and shows "Fetched full document — no excerpt available." Excerpt text is raw document content and is rendered via `escapeHTML` (never markdown) to prevent injection.

### PII rules (hard)

- Curated labels + SAFE specifics (document names, counts) only.
- `send_contact_email` records a generic label (`"Drafted a message to Anthony"`) with an **empty** target — never the Viewer's email address, draft body, or reason.
- The system prompt, the chunk-context prompt, and raw tool-result payloads are **never** stored. Only `GroundingExcerpt.Text` carries document text, which is the same `chunk.Content` already sent to the model.

---

## Relationship to ADR 0005

ADR 0005 introduced the `event: status` frame and declared status messages **transient — not persisted to session history**. This ADR **supersedes that stance**: the status frame is removed entirely and replaced by the persisted, structured trace. The escalation that ADR 0005 made possible (tool use) is now *recorded* as a `tool_call` trace step rather than shown and forgotten.

ADR 0005 also noted that citations remain chunk-derived. That remains true; the grounding reveal here is **source-level** (it surfaces the retrieved chunk excerpts behind a citation) and does not introduce claim-level mapping or similarity scores.

---

## Consequences

**Positive:**
- The agent's retrieval, escalation, and answer reasoning are now visible *and* reproducible after reload — the portfolio value the project was built to demonstrate.
- Grounding reveal lets a viewer verify that an answer is anchored in real source text, not hallucinated.
- The typed `TraceStep` allow-list makes it structurally hard to leak PII into stored traces.
- A single ordered JSONB array keeps the schema change minimal (one nullable column).

**Negative:**
- `messages.trace` duplicates retrieved chunk text into the `messages` table, growing row size (bounded by top-k=8 excerpts per assistant turn).
- Every assistant turn now writes a JSONB blob; total table growth is higher than citations-only.
- The trace reflects only what the typed structs capture — adding a new reasoning signal requires a struct/field change, not just a string.
- Legacy assistant rows have `trace = NULL` and render without a trace panel until re-answered.

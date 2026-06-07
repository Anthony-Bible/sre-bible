# ADR 0007 — Gemini Flash Lite for Source Descriptions

## Status
Accepted

## Context
When the Resume Agent calls `list_documents` it previously received only `name (type)` per
source — insufficient signal to choose between two similarly-named documents (e.g. two
resumes). We want each Source to carry a short, LLM-generated description surfaced in
`list_documents` output so the agent routes to the right document without always escalating to
`fetch_full_document`.

## Decision
During ingest, after text extraction, call `gemini-3.1-flash-lite` to produce a 1–2 sentence
(≤ ~40 word) summary of the source's content. Store it in a new nullable
`sources.description` column and include it in `list_documents` output as
`name (type): description`.

Input is capped at 12 000 runes before the API call to bound cost and latency on large URLs.

**Failure mode:** fatal — if the describe call fails, the whole ingest aborts and no source row
is written. New ingests are therefore guaranteed to have a description.

**Backfill:** none — the column is nullable; existing rows stay NULL and degrade gracefully
(`list_documents` omits the description suffix for NULL rows).

## Rationale
1. **Reuses the existing Gemini client** — no new API key, no new Go dependency.
2. **Cheapest capable model** — `gemini-3.1-flash-lite` is the lowest-cost Gemini generation
   model and a 1–2 sentence summary is a trivially small task.
3. **Stable model ID** — the `-preview` variant (`gemini-3.1-flash-lite-preview-06-17`) retires
   2026-07-09; the stable GA id is used here.
4. **Fatal-on-failure** — guarantees all new sources have routing signal; avoids silent
   degradation where agents silently fall back to the old blind-pick behaviour.

## Amendment to ADR 0003
ADR 0003 describes Gemini's role as "embeddings and PDF text extraction". This ADR extends
that boundary to include *generation for source summarisation* — a small, ingest-path-only
task. The separation of concerns between Anthropic (user-facing generation) and Gemini
(ingest/embedding) is preserved; Gemini is not used for any Viewer-facing answer generation.

## Consequences
- Ingest is slightly slower and more expensive per source (one extra `gemini-3.1-flash-lite`
  call, capped at 12 k runes input).
- All sources ingested after this change carry a description; legacy sources do not and degrade
  gracefully.
- Re-ingesting a legacy source populates its description via the `ON CONFLICT DO UPDATE` upsert.

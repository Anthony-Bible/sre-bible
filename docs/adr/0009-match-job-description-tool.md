# ADR 0009 — `match_job_description` Tool and the Fit Scorecard

**Date:** 2026-06-08  
**Status:** Accepted  
**Deciders:** Anthony Bible

---

## Context

Recruiters arrive at `sre.bible` holding a structured artifact — a Job Description —
but the Resume Agent forced them to decompose it themselves through a back-and-forth
of questions. We want a Viewer to paste a JD and get back a grounded **Fit Scorecard**:
each JD Requirement mapped to cited evidence from the Owner's ingested Sources, an
honest gap assessment per Requirement, and an overall fit summary. Secondarily this is a
portfolio demonstration of agentic engineering — task decomposition, per-Requirement
grounded Retrieval, citation discipline, and honest structured synthesis — legible to a
hiring manager in a single interaction.

This adds a fourth Tool (`match_job_description`) alongside `list_documents`,
`fetch_full_document` (ADR 0005), and `send_contact_email` (ADR 0006).

---

## Decision

Add a model-callable `match_job_description` tool. It accepts a pre-extracted
`requirements: string[]`, performs concurrent per-Requirement Retrieval, and returns a
structured JSON evidence bundle the model turns into a Fit Scorecard.

### Key decisions

- **The model decomposes, not the tool.** Tool input is `requirements: string[]`
  produced by Claude during normal reasoning — not a raw `job_description` string the
  tool splits. This avoids a secondary LLM call and a fragile Go heuristic, keeps
  decomposition visible in the model's own reasoning, and lets the model naturally ask for
  clarification on non-JD / ambiguous input instead of emitting a nonsense scorecard.
  The tool is pure per-Requirement retrieval + structured evidence assembly, with **no
  LLM call** in the adapter or handler.

- **Model-synthesis over a numeric heuristic.** Match classes (Strong / Partial / Gap)
  are assigned by the model from the returned evidence, not computed from a similarity
  score. `ChunkSearcher.SearchChunks` does not expose the distance, so a numeric
  threshold isn't available without an interface change — and a model reading the
  excerpts classifies more honestly than a distance cutoff would.

- **Single neutral Gap framing.** A Requirement with no retrieved evidence is a **Gap**,
  worded neutrally ("No supporting evidence in Anthony's documented background") — making
  no claim either way about whether the Owner has the skill. This preserves the grounding
  rule in `prompt.go` (never assert ungrounded knowledge). When any Gap exists, the model
  appends a soft CTA routing the Viewer toward `send_contact_email`.

- **Citation-flow refactor.** `runTool` now returns `(string, bool, []string)` — a slice
  of source names rather than a single name — so a tool that cites multiple Sources
  (the scorecard) folds them all into citations. The existing per-call dedup in
  `Pipeline.Answer` merges them with chunk-derived citations.

- **Bounds and logging.** At most 12 Requirements per call, each capped at 200 chars;
  each evidence excerpt capped at 300 chars; retrieval is concurrent (errgroup, limit 4)
  at k=4 per Requirement. The handler **never logs** JD or Requirement text — only
  `requirements_count`, `sources_cited`, and `duration_ms`.

- **Agent Trace integration (ADR 0008).** The tool emits exactly one `tool_call`
  Trace Step, like the other tools, with a generic PII-free label
  ("Matched the job description against Anthony's background"), an **empty target**, and
  the mapped outcome — the Viewer's pasted requirement text never reaches the persisted
  trace. (This supersedes the original transient `onStatus` per-requirement progress
  messages, which the Agent Trace feature removed in favor of persisted steps.)

- **UI: CSS only.** GFM tables already render (`marked` with `gfm:true`, DOMPurify keeps
  table tags). The only UI change is ~15 lines of table CSS in `templates/index.html` so
  the scorecard has borders, padding, a bold header row, and a horizontally scrollable
  wrapper on mobile. One Suggested Question is **replaced** (not added) so the count
  stays at 5, honoring the "3–5" glossary invariant.

---

## Consequences

**Positive:**
- Recruiters get a one-paste, grounded, cited fit analysis.
- Honest by construction: every evidence item cites a Source; Gaps make no ungrounded
  claim; the model cannot fabricate a Match from training knowledge.
- Multi-source citation support generalizes the citation flow beyond single-document
  fetch.

**Negative:**
- Streaming re-parse jank: the table rebuilds row-by-row mid-stream; only the final
  render is clean. Accepted MVP limitation.
- A JD with many requirements costs N embedding + search round-trips (bounded at 12,
  concurrency 4).
- Match-class quality depends on the model's judgment over excerpts, not a calibrated
  score.

---

## Out of scope

`book_intro_call` and `whats_anthony_doing_now` (separate issues), a typed `scorecard`
SSE event + dedicated UI component, a `job_scorecards` persistence table, and raising
the 5-round tool cap.

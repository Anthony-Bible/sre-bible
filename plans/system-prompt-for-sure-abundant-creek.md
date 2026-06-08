# Plan: PII protection — system prompt rule + ingest-time redaction

## Context

The RAG agent answers recruiter questions grounded in ingested documents (resume, brag
doc, etc.). Those source documents may contain personal PII — phone numbers, a home
address, a personal email, government IDs / DOB. Two gaps today:

1. **Behavioral gap.** The system prompt (`internal/rag/prompt.go:9`) tells the model to
   redirect *off-topic* questions to LinkedIn, but has no rule forbidding it from
   surfacing raw PII that legitimately appears in the retrieved context.
2. **Data gap.** Even a well-behaved model can't withhold what shouldn't be there. PII in
   the ingested text flows verbatim into the vector chunks, the `full_text`, and the
   `list_documents` description.

The intended outcome: PII never enters the knowledge base (defense at the data layer),
and the model is explicitly instructed never to reveal personal contact details
(defense at the behavior layer). The only contact channels the agent surfaces are
**LinkedIn, GitHub, and the `send_contact_email` tool**. The agent must **never reveal
any email address** (not even the professional one) — to contact Anthony by email, the
visitor uses the send-email tool, which delivers the message without exposing the
address.

**Decisions already made:**
- Screen at **ingest time**, not query time — chunks are static and rarely change, so
  paying per-query latency to re-screen the same content is wasteful. Re-ingest to apply
  updated rules (the architecture already supports full re-ingest via `ReplaceSource`).
- Action = **redact in place** (replace PII with `[redacted]`), preserving surrounding
  content.
- Scope = phone numbers, home/street addresses, personal email, government IDs / DOB.
- Engine = **flash-lite (LLM), not Presidio.** Presidio is the stronger PII engine but is
  Python-only and would require a sidecar service bolted onto an otherwise clean
  single-binary Go app — permanent architectural weight for a job that runs a handful of
  times on static docs. The `PIIScreener` interface keeps the door open to swap in
  Presidio later as a one-line change if high-volume/untrusted sources ever appear.

## Part 1 — System prompt rule (`internal/rag/prompt.go`)

Add an explicit PII rule to the `SystemPrompt` const (around the "When answering" block,
`prompt.go:13-17`). Wording to the effect of:

> Never reveal personal contact details — phone numbers, home or street addresses, **any
> email address (including Anthony's own)**, government IDs, or date of birth — even if
> they appear in the retrieved context. For contact, only ever share Anthony's LinkedIn
> (linkedin.com/in/anthonybible/) or his GitHub, or offer the send_contact_email tool to
> deliver the visitor's message. **Do not hand out his email address**; the send-email
> tool is the email channel. If asked for any withheld detail, politely decline and point
> to those channels.

Note: the existing redirect line (`prompt.go:19`) already points only to LinkedIn + the
send-email tool (it does not expose the email address), so it's consistent — optionally
add GitHub to it.

This belongs in the prompt regardless of Part 2 — it's the backstop if redaction ever
misses something, and it's free.

## Part 2 — Ingest-time redaction

### New gemini method (`internal/gemini/screen.go`, new file)

Mirror `Describe` (`internal/gemini/describe.go:23-42`) — reuse the existing
`generateContent` helper (`gemini.go:31-39`) with `descriptionModel`
(`"gemini-3.1-flash-lite"`):

```go
func (c *Client) ScreenPII(ctx context.Context, text string) (string, error)
```

Prompt: instruct the model to return the input text **verbatim except** that phone
numbers, home/street addresses, **all email addresses (personal and professional alike)**,
and government IDs / DOB are replaced with `[redacted]`; explicitly **allow-list**
LinkedIn and GitHub URLs so those survive (they are the only contact identifiers the agent
is permitted to surface). Output only the redacted text, no preamble.

Two deviations from `Describe`, both important:

1. **Do NOT copy the 12000-rune truncation** (`describe.go:26-30`). Describe truncates
   because it only needs a summary; redaction must cover the *entire* document or PII past
   the cutoff survives. Pass full text. (flash-lite's context window handles
   resume-sized docs comfortably.)
2. **Content-drift guard.** Asking an LLM to reproduce a long document risks it
   paraphrasing or dropping content instead of just redacting. After the call, compare
   output length to input: if the redacted text is implausibly shorter than the original
   (e.g. < ~70% of input rune count, beyond what `[redacted]` substitutions explain),
   return an error and fail the ingest rather than silently store a mangled document.
   Also keep `Describe`'s empty-output check (`describe.go:38-40`).

### New interface + wiring (`internal/ingest/pipeline.go`)

Follow the interface-at-consumption pattern used by `Describer` (`pipeline.go:30-32`):

- Add `PIIScreener interface { ScreenPII(ctx context.Context, text string) (string, error) }`
- Add a `screener PIIScreener` field to `Pipeline` (`pipeline.go:35-42`)
- Add the param to `NewPipeline` (`pipeline.go:45-54`)

### Slot-in point — `pipeline.go`, between line 68 and line 71

Screen the **full `text`** immediately after `extractText` succeeds (after the error
check at `pipeline.go:66-68`) and **before** `ChunkText` (`pipeline.go:71`):

```
text, err := p.extractText(...)   // :65
if err != nil { ... }             // :66-68
text, err = p.screener.ScreenPII(ctx, text)   // ← NEW: redact once, here
if err != nil { return fmt.Errorf("screen PII for %s: %w", name, err) }
segments := ChunkText(text)       // :71  now chunks redacted text
```

Screening `text` (the single full string) rather than the post-chunk `segments` is the
key move: `text` is the upstream source for all three sinks — `segments`→embeddings,
`Describe` input (`pipeline.go:79`), and `Source.FullText` (`pipeline.go:110`). Redact it
once and re-chunk, and redacted content flows into the vectors, the description, and the
stored full text automatically. **No changes needed to `ReplaceSource`, `db`, chunking,
or storage.**

Add a `log.InfoContext` line noting the screen ran, consistent with the existing
per-step logging.

### Wire the dependency (`cmd/ingest/main.go:60`)

`geminiClient` already satisfies the new method, so pass it once more:

```go
pipeline := ingest.NewPipeline(geminiClient, geminiClient, geminiClient, geminiClient, ingest.DefaultURLExtractor{}, store, log)
```

(Confirm arg order matches the updated `NewPipeline` signature.) Check whether
`cmd/server/main.go` also constructs an ingest `Pipeline` — if so, update that call site
too.

## Files touched

- `internal/rag/prompt.go` — add PII rule to `SystemPrompt` (Part 1)
- `internal/gemini/screen.go` — **new**, `ScreenPII` method + prompt + drift guard
- `internal/ingest/pipeline.go` — `PIIScreener` interface, field, `NewPipeline` param, screen step
- `cmd/ingest/main.go` — pass `geminiClient` as the screener arg
- `cmd/server/main.go` — only if it also builds an ingest pipeline (verify)

## Tests

- **`internal/ingest/pipeline_test.go`** — extend the existing pipeline test with a fake
  `PIIScreener`. Assert (contract, not implementation): the text handed to the embedder,
  describer, and stored `Source.FullText` is the screener's *output*, not the raw
  extracted text — i.e. redaction happens upstream of all three sinks. Add a case where
  `ScreenPII` returns an error and assert `Run` aborts without calling `ReplaceSource`.
- **`internal/gemini`** — the drift-guard length check is pure logic; if `ScreenPII` is
  structured so the guard is unit-testable without a live API call, cover the
  "output too short → error" path. (Live model calls stay out of unit tests.)

## Verification (end-to-end)

1. `make test-unit` — fake-screener pipeline tests pass.
2. Create a throwaway source containing a fake phone number, home address, a personal
   gmail, `anthony@anthonybible.com`, a fake SSN/DOB, plus a LinkedIn and GitHub URL.
3. `make ingest SRC=path/to/test.txt` with `GEMINI_API_KEY` set.
4. Inspect the DB: `chunks.content` and `sources.full_text` for that source show
   `[redacted]` in place of phone/address/**both emails**/SSN/DOB, while the LinkedIn and
   GitHub URLs survive.
5. `make query Q="What is Anthony's phone number?"` and `Q="What's his email?"` → agent
   declines and points to LinkedIn/GitHub and the send-email tool, **without revealing any
   email address** (Part 1 + Part 2 both holding).
6. Clean up the throwaway source.

# Phase 2 Plan: Core RAG + LLM

## Context

Phase 1 delivered the full data foundation: schema, ingest pipeline, Gemini client, chunker, and SourceStore. Phase 2 wires the query path: a Viewer's question (with conversation history) flows through Gemini query-embedding → pgvector cosine search → prompt assembly → Anthropic Claude streaming → cited answer. The goal is a working `cmd/query` dev tool that proves the end-to-end RAG loop before any web layer is built.

---

## Architecture: Package Layering

Hexagonal rule strictly preserved — domain packages have zero internal imports, adapters import domain.

```
cmd/query
  → internal/rag     (NEW domain — zero internal imports)
  → internal/db      (adapter — imports internal/ingest AND internal/rag)
  → internal/gemini  (adapter — zero internal imports)
  → internal/llm     (NEW adapter — zero internal imports)
```

`internal/db` importing `internal/rag` follows the same logic as it importing `internal/ingest` — adapter importing domain.

---

## Grill-Resolved Design Decisions

| Decision | Resolution |
|---|---|
| Multi-turn support | Yes — `Pipeline.Answer` takes `history []Message`; caller (Phase 3 web server) loads history from DB |
| Context injection | Fresh retrieval per question; context injected only into the current user message, not re-injected into history |
| Generator interface | Takes fully assembled `[]Message` (pipeline owns prompt assembly; Generator is a dumb streamer) |
| System prompt persona | "senior Site Reliability Engineer and platform engineering leader" |
| Off-topic redirect | Option C: "I'm focused on Anthony's professional background. For anything else, you can reach him directly at linkedin.com/in/anthonybible/." |
| `cmd/query` scope | Permanent dev/debug tool — useful for post-ingest retrieval verification |

---

## Implementation Workflow: TDD Per Layer

Apply red→green→refactor→review cycle in this order:

1. `internal/rag` — domain (interfaces, types, pipeline, prompt)
2. `internal/gemini` — add `EmbedQuery`
3. `internal/db` — add `SearchChunks`
4. `internal/llm` — Anthropic streaming client
5. `cmd/query` — wiring + compile-time assertions

For each layer:
- **red-phase-tester**: write failing tests first
- **green-phase-implementer**: minimal code to make tests pass
- **tdd-refactor-specialist**: clean up while keeping tests green
- **tdd-review-agent**: verify no skipped tests, no inappropriate mocks

---

## Step 0 — Before Any Code

1. Update `CONTEXT.md` — add "Message" to the glossary:
   > **Message** — A single conversational turn within a Session. Each Message has a role (`user` or `assistant`) and a text content. User Messages contain the Viewer's question. Assistant Messages contain the Resume Agent's response. Messages are persisted to Cloud SQL and are visible to the Owner for analytics.

2. Add Anthropic SDK:
   ```
   go get github.com/anthropics/anthropic-sdk-go
   go mod tidy
   ```

---

## Files to Create / Modify

### 1. `internal/rag/domain.go` — NEW

Zero internal imports. All three interfaces plus the two value types.

```go
package rag

import "context"

// Role is the participant in a conversation turn.
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
)

// Message is a single conversational turn in a Session.
type Message struct {
    Role    Role
    Content string
}

// RetrievedChunk is a Chunk recovered from the vector store during Retrieval,
// annotated with its Source citation name for attribution.
type RetrievedChunk struct {
    Content    string
    SourceName string // e.g. "resume.pdf" or "https://..."
}

// QueryEmbedder converts a Viewer's question into a 768-dim query vector.
// MUST use RETRIEVAL_QUERY task type — not RETRIEVAL_DOCUMENT.
type QueryEmbedder interface {
    EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// ChunkSearcher finds the k most semantically similar RetrievedChunks via cosine similarity.
type ChunkSearcher interface {
    SearchChunks(ctx context.Context, queryEmbedding []float32, limit int) ([]RetrievedChunk, error)
}

// Generator streams a grounded answer token by token via callback.
// It receives fully assembled messages (history + enriched current turn).
// If onToken returns an error the stream must abort and that error is returned.
type Generator interface {
    StreamAnswer(ctx context.Context, messages []Message, onToken func(string) error) error
}
```

**Note:** `Generator` no longer takes `systemPrompt` as a param — the system prompt is baked into the `llm.Client` at construction time (passed to `NewClient`). This keeps the system prompt out of the hot path and makes `internal/llm` self-contained.

### 2. `internal/rag/prompt.go` — NEW

System prompt constant + prompt-builder helpers.

```go
package rag

const SystemPrompt = `You are the Resume Agent for Anthony Bible, a senior Site Reliability Engineer and platform engineering leader.

Your knowledge comes exclusively from the documents and web pages ingested into your knowledge base. Do NOT answer from general knowledge or training data. If the provided context does not contain enough information, say so clearly.

When answering:
- Be direct and specific; skip filler like "Based on the context provided..."
- Write in third person about Anthony (e.g. "Anthony has led..." not "I have led...")
- Do not include footnotes, citations, or source references in your answer text; those are appended separately
- Keep answers concise unless depth is warranted

If a question is unrelated to Anthony Bible's professional background, politely redirect: "I'm focused on Anthony's professional background. For anything else, you can reach him directly at linkedin.com/in/anthonybible/."`

// BuildContextBlock formats retrieved chunks as an XML-tagged block.
// Each chunk is labelled with its source name.
func BuildContextBlock(chunks []RetrievedChunk) string

// BuildUserMessage returns the final user Message for the current turn:
// the context block followed by the Viewer's question.
// This is the last element appended to the history slice before calling Generator.
func BuildUserMessage(question string, chunks []RetrievedChunk) Message
```

Context block format per chunk:
```xml
<chunk source="resume.pdf" index="0">
chunk content here
</chunk>
```

Full user message format:
```
<context>
<chunk source="resume.pdf" index="0">...</chunk>
...
</context>

Question: <question text>
```

### 3. `internal/rag/pipeline.go` — NEW

```go
package rag

const defaultK = 8

type Pipeline struct {
    embedder  QueryEmbedder
    searcher  ChunkSearcher
    generator Generator
    k         int
    log       *slog.Logger
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, k int, log *slog.Logger) *Pipeline

// Answer embeds the question, retrieves relevant chunks, assembles the full
// message history, streams a grounded response via onToken, and returns
// deduplicated citation source names.
//
// history contains prior turns from the Session (may be empty for first turn).
// citations are returned after streaming completes; they are derived from
// retrieved chunks, not from Claude's output.
func (p *Pipeline) Answer(ctx context.Context, history []Message, question string, onToken func(string) error) (citations []string, err error)
```

**`Answer` internal logic:**
1. `EmbedQuery(ctx, question)` → `queryVec`
2. `SearchChunks(ctx, queryVec, p.k)` → `chunks`
3. **Empty-context guard:** if `len(chunks) == 0`, call `onToken` with `"I couldn't find relevant information in my knowledge base to answer that question."`, return empty citations — do NOT call Generator
4. `BuildUserMessage(question, chunks)` → `currentMsg`
5. Assemble: `messages := append(history, currentMsg)` — shallow copy; do not mutate the input slice
6. `generator.StreamAnswer(ctx, messages, onToken)`
7. Deduplicate SourceName fields from `chunks` preserving first-seen order → `citations`
8. `log.InfoContext(ctx, "query answered", "chunks", len(chunks), "citations", len(citations))`
9. Return `citations, err`

**Citation deduplication (order-preserving):**
```go
seen := make(map[string]struct{})
for _, c := range chunks {
    if _, ok := seen[c.SourceName]; !ok {
        seen[c.SourceName] = struct{}{}
        citations = append(citations, c.SourceName)
    }
}
```

### 4. `internal/rag/pipeline_test.go` — NEW

Unit tests — in-process stubs, no DB, no network. `package rag_test`, `t.Parallel()`, no testify.

Tests:
- `TestPipeline_EmptyChunkGuard` — zero chunks → canned message via onToken, empty citations, Generator never called
- `TestPipeline_CitationDeduplication` — chunks from two sources, one repeated → citations has each name once, insertion order preserved
- `TestPipeline_HistoryPassedToGenerator` — non-empty history → generator receives history prepended to current message
- `TestBuildContextBlock` — verify XML structure, source attribute, index attribute
- `TestBuildUserMessage` — context block precedes "Question:", role is RoleUser

### 5. `internal/gemini/embed.go` — MODIFY (add `EmbedQuery`)

New method satisfying `rag.QueryEmbedder`.

```go
// EmbedQuery returns a 768-dim embedding for a single query text.
// Uses RETRIEVAL_QUERY task type — distinct from RETRIEVAL_DOCUMENT used at ingest time.
// Gemini's asymmetric design makes cross-task cosine similarity correct and intended.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error)
```

Implementation mirrors `EmbedDocuments`:
- Single `*genai.Content` (not a batch)
- `TaskType: "RETRIEVAL_QUERY"`
- Returns `resp.Embeddings[0].Values`; errors if `len(resp.Embeddings) == 0`

### 6. `internal/db/store.go` — MODIFY (add `SearchChunks`)

New method + new import + compile-time assertion.

```go
// SearchChunks returns the limit most semantically similar RetrievedChunks
// for queryEmbedding, ordered by ascending cosine distance (most similar first).
func (s *SourceStore) SearchChunks(ctx context.Context, queryEmbedding []float32, limit int) ([]rag.RetrievedChunk, error)
```

**SQL:**
```sql
SELECT c.content, s.name
FROM   chunks c
JOIN   sources s ON s.id = c.source_id
ORDER  BY c.embedding <=> $1
LIMIT  $2
```
- `$1` = `pgvector.NewVector(queryEmbedding)` — OIDs already registered via pool's `AfterConnect`
- `<=>` = pgvector cosine distance; ascending = most similar first
- HNSW index on `chunks(embedding vector_cosine_ops)` used automatically by planner

**New import:** `github.com/Anthony-Bible/sre-bible/internal/rag`

**Compile-time assertion** (alongside existing `ingest.SourceRepository` one):
```go
var _ rag.ChunkSearcher = (*SourceStore)(nil)
```

**New integration test** `TestSearchChunks` in `internal/db/store_test.go`:
- Ingest two sources with `makeEmbedding(1.0)` and `makeEmbedding(100.0)`
- Search with `makeEmbedding(1.1)` (close to seed 1.0)
- Assert `results[0].SourceName` matches the seed-1.0 source
- Assert `len(results) >= 1`

### 7. `internal/llm/llm.go` — NEW

Single file. Wraps `github.com/anthropics/anthropic-sdk-go`. Satisfies `rag.Generator`.

```go
package llm

type Client struct {
    inner        *anthropic.Client
    model        string
    systemPrompt string
    log          *slog.Logger
}

// NewClient creates an Anthropic Claude streaming client.
// systemPrompt is sent on every call; model is e.g. "claude-haiku-4-5-20251001".
func NewClient(apiKey, model, systemPrompt string, log *slog.Logger) *Client

// StreamAnswer implements rag.Generator. Sends systemPrompt + messages to Claude,
// invoking onToken for each text delta. Aborts if onToken returns an error.
func (c *Client) StreamAnswer(ctx context.Context, messages []rag.Message, onToken func(string) error) error
```

**`messages` → Anthropic params translation:**
```go
params := make([]anthropic.MessageParam, len(messages))
for i, m := range messages {
    switch m.Role {
    case rag.RoleUser:
        params[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
    case rag.RoleAssistant:
        params[i] = anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content))
    }
}
```

**Streaming pattern** (verify exact event API against `go doc` when implementing — SDK version determines exact type switch):
```go
stream := c.inner.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
    Model:     anthropic.Model(c.model),
    MaxTokens: 2048,
    System:    []anthropic.TextBlockParam{{Text: c.systemPrompt}},
    Messages:  params,
})
for stream.Next() {
    // extract TextDelta from stream.Current() → call onToken
}
return stream.Err()
```

`MaxTokens: 2048` is sufficient for resume Q&A. The system prompt is stored on the client (not passed per-call) because it never changes between requests.

**Note:** `internal/llm` imports `internal/rag` to use `rag.Message` and `rag.Role`. This is an adapter importing a domain package — correct per hexagonal rules.

### 8. `cmd/query/main.go` — NEW (permanent dev/debug tool)

Mirrors `cmd/ingest/main.go` pattern. Single-turn query only (passes empty history) — sufficient for retrieval verification.

**Env vars:** `DATABASE_URL`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`
**Usage:** `query "<question>"`

**Compile-time interface assertions** (here because `internal/gemini` doesn't import `internal/rag`):
```go
var (
    _ rag.QueryEmbedder = (*gemini.Client)(nil)
    _ rag.ChunkSearcher = (*db.SourceStore)(nil)
    _ rag.Generator     = (*llm.Client)(nil)
)
```

**Wiring:**
```go
pool    := db.NewPool(ctx, dbURL, log)
store   := db.NewSourceStore(pool, log)
gemCli  := gemini.NewClient(ctx, geminiKey, log)
llmCli  := llm.NewClient(anthropicKey, "claude-haiku-4-5-20251001", rag.SystemPrompt, log)
pipe    := rag.NewPipeline(gemCli, store, llmCli, 0, log)

citations, err := pipe.Answer(ctx, nil, question, func(tok string) error {
    fmt.Print(tok)
    return nil
})
fmt.Printf("\n\n--- Sources ---\n")
for _, c := range citations {
    fmt.Printf("  [%s]\n", c)
}
```

### 9. `go.mod` — MODIFY

```
go get github.com/anthropics/anthropic-sdk-go
go mod tidy
```

### 10. `Makefile` — MODIFY

```makefile
query:
	@if [ -z "$(Q)" ]; then echo "Usage: make query Q=\"<question>\""; exit 1; fi
	DATABASE_URL=$(DATABASE_URL) GEMINI_API_KEY=$(GEMINI_API_KEY) ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY) \
		go run ./cmd/query "$(Q)"
```

---

## Key Reuse Notes

- `internal/gemini.Client` reused as-is; only `EmbedQuery` added
- `db.NewPool` and `db.NewSourceStore` reused unchanged
- `pgvector.NewVector()` encoding from `insertChunks` reused in `SearchChunks`
- `testDB` and `makeEmbedding` helpers from `store_test.go` reused for `TestSearchChunks`

---

## Verification

1. `make db-up && make migrate` — schema current
2. `go mod tidy` — dependency graph clean, no cycles
3. `make test` — all existing tests pass; new `internal/rag` unit tests pass
4. `make test-integration` — `TestSearchChunks` passes against live DB
5. `make ingest SRC=<resume.pdf>` — at least one source ingested
6. `make query Q="What cloud platforms has Anthony worked with?"` — streaming answer + sources to stdout

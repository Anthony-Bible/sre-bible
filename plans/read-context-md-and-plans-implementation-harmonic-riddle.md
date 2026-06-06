# Phase 3 Plan — Web Server

## Context

Phases 1 and 2 are complete: the ingest pipeline, pgvector store, Gemini embedding client, Anthropic streaming LLM client, and `internal/rag.Pipeline.Answer()` are all wired and tested. Phase 3 adds an HTTP server that exposes the RAG pipeline as a streaming chat UI at `sre.bible`. A new migration adds `citations TEXT[]` to the `messages` table. Session rows are created lazily on first message, not on page load — every persisted session has at least one message, keeping analytics meaningful.

---

## Architecture

```
Browser ──POST /chat──► cmd/server ──► internal/server (handlers)
                                            │
                          ┌─────────────────┼─────────────────┐
                          ▼                 ▼                 ▼
               internal/rag          internal/db        html/template
               (Pipeline.Answer)    (SessionStore)      (index.html)
                    │
          ┌─────────┼─────────┐
          ▼         ▼         ▼
      gemini/     db/       llm/
    EmbedQuery SearchChunks StreamAnswer
```

`internal/server` depends on `internal/rag` types only. `SessionRepository` and `Answerer` interfaces are defined in `internal/server/server.go` (consumed-side, per Go interface guidelines). `db.SessionStore` satisfies `SessionRepository` via duck typing; the compile-time assertion lives in `cmd/server/main.go` to avoid import cycles.

---

## SSE Protocol

`POST /chat` is both the message endpoint and the SSE stream. The handler sets `Content-Type: text/event-stream` and streams named events. The browser uses `fetch` + `ReadableStream` (not `EventSource`, which is GET-only) — the session cookie travels automatically with the POST, no `?sid=` URL parameter needed.

```
event: token
data: {"t":"Hello"}

event: token
data: {"t":", Anthony"}

event: done
data: {"citations":["resume.pdf","anthonybible.com/about"]}

event: error
data: {"msg":"failed to generate response"}
```

Progressive markdown rendering with `marked.js` requires accumulating raw tokens and re-rendering on each token — this is why vanilla JS `fetch` is used rather than a fragment-swapping approach.

---

## Files to Create / Modify

### 1. `internal/rag/domain.go` — no changes

`Pipeline.Answer` takes `[]Message` directly. Session persistence is a server concern; the interface lives in `internal/server`.

### 2. `internal/db/migrations/0002_add_citations.sql` — new migration

```sql
-- +goose Up
ALTER TABLE messages ADD COLUMN citations TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE messages DROP COLUMN citations;
```

### 3. `internal/db/session.go` — `SessionStore` (new file, `db` package)

Satisfies `server.SessionRepository` via duck typing.

```go
type SessionStore struct {
    pool   *pgxpool.Pool
    logger *slog.Logger
}

func NewSessionStore(pool *pgxpool.Pool, logger *slog.Logger) *SessionStore

func (s *SessionStore) CreateSession(ctx context.Context, sessionID string) error
// INSERT INTO sessions (id) VALUES ($1) ON CONFLICT DO NOTHING

func (s *SessionStore) ListMessages(ctx context.Context, sessionID string) ([]server.StoredMessage, error)
// SELECT role, content, citations FROM messages
// WHERE session_id = $1 ORDER BY created_at ASC
// Scan: role → rag.Role, content → string, citations → []string

func (s *SessionStore) AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string) error
// INSERT INTO messages (session_id, role, content, citations) VALUES ($1, $2, $3, $4)
// User messages: pass nil citations (stored as '{}')
```

### 4. `internal/server/server.go` — Server struct, interfaces, routes

```go
package server

import "embed"

//go:embed templates
var templateFS embed.FS

// Answerer is the port for streaming answers. Satisfied by *rag.Pipeline.
type Answerer interface {
    Answer(ctx context.Context, history []rag.Message, question string, onToken func(string) error) ([]string, error)
}

// StoredMessage is a Message with its persisted citation list, used for page rendering.
type StoredMessage struct {
    rag.Message          // embeds Role + Content
    Citations []string
}

// SessionRepository is the persistence port for anonymous chat sessions.
// Defined here (consumed here); implemented by *db.SessionStore.
// Compile-time assertion in cmd/server/main.go.
type SessionRepository interface {
    CreateSession(ctx context.Context, sessionID string) error
    ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error)
    AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string) error
}

type Server struct {
    pipeline  Answerer
    sessions  SessionRepository
    templates *template.Template
    log       *slog.Logger
    mux       *http.ServeMux
}

func NewServer(pipeline Answerer, sessions SessionRepository, log *slog.Logger) (*Server, error)
// - parse templates with FuncMap{"add": func(a, b int) int { return a + b }}
// - register: mux.HandleFunc("GET /", s.handleIndex)
// - register: mux.HandleFunc("POST /chat", s.handleChat)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)

var suggestedQuestions = []string{
    "What is Anthony's experience with Kubernetes and GKE?",
    "How has Anthony approached platform reliability at scale?",
    "What does Anthony's incident management background look like?",
    "What SRE practices has Anthony championed in past roles?",
}
```

### 5. `internal/server/handlers.go`

**Template data structs:**
```go
type chatData struct {
    Messages           []renderedMessage
    ShowSuggestions    bool
    SuggestedQuestions []string
}
type renderedMessage struct {
    Role      string   // "user" or "assistant"
    Content   string   // raw text (user) or raw markdown (assistant) — marked.js renders client-side
    Citations []string // non-nil only for assistant messages
}
```

**`handleIndex` (GET /)**

1. Read `session_id` cookie via `sessionFromRequest(r)`.
2. If absent: `newSessionID()`, then `setSessionCookie(w, sid)`.
3. `sessions.ListMessages(ctx, sid)` → `[]StoredMessage`.
4. Convert to `[]renderedMessage` (user messages: plain content, no citations; assistant: raw markdown + citations).
5. Render `index.html` with `chatData{..., ShowSuggestions: len(stored)==0}`.

**`handleChat` (POST /chat)**

1. `r.ParseForm()`.
2. Read cookie → if absent, generate UUID and set cookie.
3. `question := strings.TrimSpace(r.FormValue("question"))`.
4. If question is empty: `http.Error(w, "question required", http.StatusBadRequest)` — return BEFORE setting SSE headers.
5. Set SSE headers:
   ```
   Content-Type: text/event-stream
   Cache-Control: no-cache
   X-Accel-Buffering: no
   ```
6. Assert `http.Flusher` — if not satisfied, 500 and return.
7. `sessions.CreateSession(ctx, sid)` — idempotent (ON CONFLICT DO NOTHING).
8. Load history: `sessions.ListMessages(ctx, sid)` → extract `[]rag.Message` for pipeline:
   ```go
   history := make([]rag.Message, len(stored))
   for i, sm := range stored { history[i] = sm.Message }
   ```
9. `sessions.AppendMessage(ctx, sid, rag.Message{Role: rag.RoleUser, Content: question}, nil)`.
10. Stream:
    ```go
    var buf strings.Builder
    citations, err := s.pipeline.Answer(r.Context(), history, question, func(tok string) error {
        buf.WriteString(tok)
        return sseToken(w, flusher, tok)
    })
    ```
11. On error: `sseError(w, flusher, "failed to generate response")` + log. Do NOT persist partial assistant message.
12. On success — use detached context so DB write survives browser disconnect:
    ```go
    persistCtx := context.WithoutCancel(r.Context())
    sessions.AppendMessage(persistCtx, sid, rag.Message{Role: rag.RoleAssistant, Content: buf.String()}, citations)
    sseDone(w, flusher, citations)
    ```

### 6. `internal/server/session.go`

```go
const cookieName = "session_id"
const cookieMaxAge = 30 * 24 * 60 * 60 // 30 days in seconds

// newSessionID generates a UUID v4 using crypto/rand. No external package.
// read 16 bytes, set version (b[6]) and variant (b[8]) bits, format as 8-4-4-4-12 hex.
func newSessionID() (string, error)

// sessionFromRequest returns the session ID from the cookie, or "" if absent.
func sessionFromRequest(r *http.Request) string

// setSessionCookie writes an HttpOnly, SameSite=Lax session cookie to w.
func setSessionCookie(w http.ResponseWriter, id string)
```

### 7. `internal/server/sse.go`

```go
// writeSSE writes "event: <name>\ndata: <json>\n\n" and flushes.
func writeSSE(w http.ResponseWriter, f http.Flusher, event string, payload any) error

func sseToken(w http.ResponseWriter, f http.Flusher, token string) error
func sseDone(w http.ResponseWriter, f http.Flusher, citations []string) error  // never sends null citations
func sseError(w http.ResponseWriter, f http.Flusher, msg string) error
```

Unexported payload structs: `tokenPayload{T string}`, `donePayload{Citations []string}`, `errorPayload{Msg string}`.

### 8. `internal/server/templates/index.html`

Single Go template file. Key structure:
- **Header**: Name + title (placeholder bio for Phase 4 polish).
- **`#chat-log`**: History from `{{range .Messages}}`. User bubbles: `<div class="msg-body">{{.Content}}</div>`. Assistant bubbles: `<div class="msg-body" data-content="{{.Content}}"></div>` + citation footnotes.
- **`#suggestions`**: `{{if .ShowSuggestions}}` — 4 buttons, each calls `submitQuestion(this.textContent)`.
- **`#chat-form`**: `<form id="chat-form" onsubmit="handleSubmit(event)">` with `<textarea name="question">` and Send button.
- **`#streaming-bubble`** (hidden): target for live token output.
- **`<script>`**: vanilla JS (see below).

Inline CSS only — no external CSS framework. Mobile-responsive flex column layout.

marked.js via CDN:
```html
<script src="https://cdn.jsdelivr.net/npm/marked/marked.min.js"></script>
```

**Page-load history rendering** (runs once on load, before any streaming):
```js
document.querySelectorAll('.msg-body[data-content]').forEach(el => {
    el.innerHTML = marked.parse(el.dataset.content);
});
```

**Streaming JS sketch** (~60 lines in `<script>`):
```javascript
async function handleSubmit(e) {
    e.preventDefault();
    const q = document.getElementById('question-input').value.trim();
    if (!q) return;
    document.getElementById('question-input').value = '';
    document.getElementById('suggestions')?.remove();
    appendUserBubble(q);
    showStreamingBubble();

    const resp = await fetch('/chat', {
        method: 'POST',
        headers: {'Content-Type': 'application/x-www-form-urlencoded'},
        body: new URLSearchParams({question: q}),
    });
    if (!resp.ok) { showError('Request failed'); return; }

    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = '', accum = '';
    while (true) {
        const {done, value} = await reader.read();
        if (done) break;
        buf += dec.decode(value, {stream: true});
        const parts = buf.split('\n\n');
        buf = parts.pop();
        for (const chunk of parts) {
            let event = 'message', data = '';
            for (const line of chunk.split('\n')) {
                if (line.startsWith('event: ')) event = line.slice(7).trim();
                else if (line.startsWith('data: ')) data = line.slice(6).trim();
            }
            if (event === 'token') {
                accum += JSON.parse(data).t;
                updateStreamingBubble(marked.parse(accum));
            } else if (event === 'done') {
                finalizeStreamingBubble(accum, JSON.parse(data).citations);
            } else if (event === 'error') {
                showError(JSON.parse(data).msg);
            }
        }
    }
}

function submitQuestion(text) {
    document.getElementById('question-input').value = text;
    document.getElementById('chat-form')
        .dispatchEvent(new Event('submit', {bubbles: true, cancelable: true}));
}
```

### 9. `cmd/server/main.go`

Env vars: `DATABASE_URL` (required), `GEMINI_API_KEY` (required), `ANTHROPIC_API_KEY` (required), `CLAUDE_MODEL` (default: `claude-haiku-4-5`), `LISTEN_ADDR` (default: `:8080`).

Wire-up order: `db.NewPool` → `db.Migrate` → `gemini.NewClient` → `db.NewSourceStore` → `db.NewSessionStore` → `llm.NewClient` → `rag.NewPipeline` → `server.NewServer` → `http.ListenAndServe`.

Compile-time assertion (avoids import cycle between `db` and `server`):
```go
var _ server.SessionRepository = (*db.SessionStore)(nil)
```

```go
httpSrv := &http.Server{
    Addr:              addr,
    Handler:           srv,
    ReadHeaderTimeout: 10 * time.Second,
    IdleTimeout:       120 * time.Second,
    // WriteTimeout intentionally omitted — SSE streams are long-lived
}
```

### 10. `Makefile` additions

```makefile
PORT ?= 8080

serve: db-up
	DATABASE_URL=$(DATABASE_URL) GEMINI_API_KEY=$(GEMINI_API_KEY) \
	ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY) LISTEN_ADDR=:$(PORT) \
	go run ./cmd/server

build-server:
	go build -o bin/server ./cmd/server

.PHONY: serve build-server
```

---

## Key Reused Functions

| Function | File | Usage |
|---|---|---|
| `db.NewPool` | `internal/db/db.go` | Pool in `cmd/server/main.go` |
| `db.Migrate` | `internal/db/migrate.go` | Runs migrations on startup (picks up `0002_add_citations.sql`) |
| `db.NewSourceStore` | `internal/db/store.go` | `ChunkSearcher` for `rag.NewPipeline` |
| `rag.NewPipeline` | `internal/rag/pipeline.go` | Core RAG pipeline |
| `rag.SystemPrompt` | `internal/rag/prompt.go` | Passed to `llm.NewClient` |
| `gemini.NewClient` | `internal/gemini/gemini.go` | Embedding client |
| `llm.NewClient` | `internal/llm/llm.go` | Anthropic streaming client |

---

## TDD Implementation Order

Apply the full red → green → refactor → review cycle for each component below, in order:

| # | Component | Red agent | Green agent | Refactor agent | Review agent |
|---|---|---|---|---|---|
| 1 | `internal/db/session.go` + migration | `red-phase-tester` | `green-phase-implementer` | `tdd-refactor-specialist` | `tdd-review-agent` |
| 2 | `internal/server/session.go` (UUID + cookie helpers) | `red-phase-tester` | `green-phase-implementer` | `tdd-refactor-specialist` | `tdd-review-agent` |
| 3 | `internal/server/sse.go` (wire helpers) | `red-phase-tester` | `green-phase-implementer` | `tdd-refactor-specialist` | `tdd-review-agent` |
| 4 | `internal/server/server.go` + `handlers.go` (full HTTP flow) | `red-phase-tester` | `green-phase-implementer` | `tdd-refactor-specialist` | `tdd-review-agent` |
| 5 | `cmd/server/main.go` (wiring only — no unit tests; verify with `go build`) | — | `green-phase-implementer` | — | `tdd-review-agent` |

Complete all four phases for component N before starting component N+1 (later components build on earlier ones).

---

## Verification

**Unit tests:**
- `internal/server/session_test.go`: `TestNewSessionID_Format` (UUID regex `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), `TestNewSessionID_Uniqueness` (1000 IDs, no duplicates in map).
- `internal/server/sse_test.go`: `writeSSE` output format assertions using a `httptest.ResponseRecorder` + custom `Flusher` wrapper.

**Integration tests:**
- `internal/db/session_test.go`: `CreateSession` → `ListMessages` (empty) → `AppendMessage` (user, nil citations) → `AppendMessage` (assistant, citations) → `ListMessages` (returns both with correct citations). `CreateSession` twice is idempotent. Reuse `testDB` helper pattern from `store_test.go`.

**Manual end-to-end:**
```bash
make db-up && make migrate           # picks up 0002_add_citations.sql
make ingest SRC=resume.pdf
make serve
# open http://localhost:8080
# 4 suggestion buttons visible
# click a suggestion → buttons disappear, stream arrives with markdown + citations
# ask a follow-up → history carries into LLM context
# reload page → cookie persists, history visible with citations
# DevTools → Network → /chat → EventStream tab: token/done events visible
```

**LSP gate:** After each new file, run LSP diagnostics. Fix type errors and missing imports before moving to the next file.

# Per-Tab Sessions via sessionStorage

## Context

Chat sessions currently persist for 30 days via an HttpOnly cookie (`internal/server/session.go:13`). On every `GET /` the server reads the cookie and re-renders the full conversation history from Postgres, so the conversation survives reloads, navigation, and even browser restarts.

Desired behavior: **per-tab sessions**. A session survives reloads of the same tab, but a new tab or closing the tab starts a fresh conversation. Since cookies can't be scoped per-tab, the session ID moves to `sessionStorage` and travels on requests via an `X-Session-ID` header. Client-side only — no DB purge; orphaned Postgres rows are acceptable.

## Design

- **ID generation**: client-side `crypto.randomUUID()`, stored in `sessionStorage` under `sre_session_id`. Server stops minting IDs entirely.
- **Transport**: `X-Session-ID` header on `POST /chat` and a new `GET /messages` (the chat submit already uses `fetch` with `URLSearchParams`, so adding a header is trivial).
- **History restore**: `GET /` becomes a static shell (no DB call). On `DOMContentLoaded`, JS fetches `GET /messages` with the stored ID and rebuilds bubbles using the existing SSE DOM builders (`appendUserBubble`, `finalizeStreamingBubble`) so markdown + citation rendering stays identical.
- **Validation**: session ID is now untrusted client input → both endpoints validate canonical UUID v4 shape before any DB call, else 400.

## Security notes (cookie → sessionStorage + header)

- **CSRF improves**: a custom `X-Session-ID` header can't be sent by cross-origin forms/simple requests (CORS preflight required), unlike auto-attached cookies. Stronger than the previous `SameSite=Lax` mitigation.
- **Trade-off**: sessionStorage is JS-readable (HttpOnly cookie wasn't), so XSS could steal the ID — but with no accounts/auth, the ID only guards a chat transcript that an XSS payload could read from the DOM anyway. Accepted.
- **Entropy unchanged**: `crypto.randomUUID()` is CSPRNG-backed, same 122 bits as the old server-side `crypto/rand` minting; server-side UUIDv4 regex validation rejects malformed input before any DB call.
- **No ID in URLs**: header (not query param) on `GET /messages` keeps the ID out of access logs and browser history.
- **Pre-existing, unchanged**: `marked.parse` renders LLM output as raw HTML (index.html:357,437); server-side history rows never expire, so a leaked UUID can fetch its history indefinitely (DB purge explicitly out of scope).

## Changes

### 1. `internal/server/session.go` (rewrite)

- Delete `cookieName`, `cookieMaxAge`, `newSessionID`, `setSessionCookie`, and the `crypto/rand`/`encoding/hex`/`fmt`/`io` imports.
- `const sessionHeader = "X-Session-ID"`.
- `sessionFromRequest(r)` → `r.Header.Get(sessionHeader)`.
- New `validSessionID(id string) bool` matching `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` (same shape `uuidV4Re` in `session_test.go` asserts). Compile the regexp via `sync.OnceValue` or a func accessor to match the repo's `gochecknoglobals` posture.

### 2. `internal/server/server.go`

- Register route: `mux.HandleFunc("GET /messages", s.handleMessages)`.
- If the `add` template func was only used by the deleted `{{range .Messages}}` citations block, remove it.

### 3. `internal/server/handlers.go`

- **`handleIndex`** (lines 52-92): remove cookie read/mint and `ListMessages` call. Render `chatData{ShowSuggestions: true, SuggestedQuestions: defaultSuggestedQuestions()}`. Drop the `Messages` field from `chatData` and the now-unused `renderedMessage` (the JSON endpoint gets its own DTO).
- **New `handleMessages`** (`GET /messages`):
  - `sid := sessionFromRequest(r)`; `!validSessionID(sid)` → `http.Error(..., http.StatusBadRequest)`.
  - `s.sessions.ListMessages(ctx, sid)`; error → 500 with `s.log.ErrorContext(...)` in existing style.
  - Respond `application/json` array: `[{"role":"user","content":"...","citations":[]}, ...]` — DTO with lowercase JSON tags; normalize nil citations to `[]`.
- **`handleChat`** (lines 96-181): replace the cookie-or-mint block (lines 102-111) with `if !validSessionID(sid) → 400`, placed alongside the empty-question guard — both before SSE headers are written so the JS `!resp.ok` branch handles it cleanly. Everything downstream (`CreateSession` upsert, history load, appends, streaming) unchanged.

### 4. `internal/server/templates/index.html`

- Delete the server-rendered `{{range .Messages}}` block (lines 235-251) — `#chat-log` starts empty.
- Remove the `{{if .ShowSuggestions}}`/`{{end}}` wrapper (suggestions always render in the shell; JS removes them when history restores non-empty, and `handleSubmit` already removes them on first send at line 393).
- Delete the `data-content` `marked.parse` loop in `DOMContentLoaded` (lines 281-283).
- Add:
  ```js
  function getSessionId() {
      let id = sessionStorage.getItem('sre_session_id');
      if (!id) { id = crypto.randomUUID(); sessionStorage.setItem('sre_session_id', id); }
      return id;
  }

  async function restoreHistory() {
      let resp;
      try { resp = await fetch('/messages', { headers: { 'X-Session-ID': getSessionId() } }); }
      catch { return; }
      if (!resp.ok) return;
      const msgs = await resp.json();
      if (!Array.isArray(msgs) || msgs.length === 0) return;
      document.getElementById('suggestions')?.remove();
      for (const m of msgs) {
          if (m.role === 'user') appendUserBubble(m.content);
          else finalizeStreamingBubble(m.content, m.citations || []);
      }
  }
  ```
  Call `restoreHistory()` from `DOMContentLoaded`. (`finalizeStreamingBubble`'s side effects — `clearStatus`, hiding the streaming bubble, re-enabling send-btn — are no-ops at load.)
- In `handleSubmit`'s fetch (line 401-405), add `'X-Session-ID': getSessionId()` to headers.
- Caveat: `crypto.randomUUID()` requires a secure context (HTTPS or localhost) — fine for this deployment.

## Tests

Update (cookie → header):
- `session_test.go`: delete `TestNewSessionID_*` and `TestSetSessionCookie_WritesHeader`; rewrite `TestSessionFromRequest_*` for the header.
- `handlers_test.go`: in all `TestHandleChat_*` tests, replace `req.AddCookie(...)` with `req.Header.Set("X-Session-ID", "<valid v4 uuid>")` (existing literals like `aabbccdd-0000-4000-8000-000000000004` already pass validation). Fold `TestHandleIndex_NewVisitor`/`_ReturningVisitor` into one shell test (200, no Set-Cookie, no ListMessages call); move `TestHandleIndex_ListMessagesFails` contract to the new endpoint.

New (contract-level):
- `TestValidSessionID` — table-driven: valid v4 / non-UUID / wrong version nibble / empty / uppercase.
- `TestHandleMessages_BadSessionID` — missing/malformed header → 400, no DB call.
- `TestHandleMessages_HappyPath` — stub returns user (nil citations) + assistant (`["a.pdf"]`); assert 200, `application/json`, nil normalized to `[]`.
- `TestHandleMessages_ListMessagesFails` — stub error → 500.
- `TestHandleChat_BadSessionID` — malformed header + valid question → 400, `len(sessions.appended) == 0`.

No `internal/db` changes — `CreateSession`/`ListMessages`/`AppendMessage` are untouched.

## Verification

```
go build ./... && go vet ./... && go test ./internal/server/...
```

Manual (run server locally):
1. Tab A: ask a question → streamed answer with citations. Reload tab A → history reappears identically, suggestions gone; DevTools shows `sre_session_id` in Session Storage and `GET /messages` + `POST /chat` carrying `X-Session-ID`.
2. New tab B → empty chat, suggestions shown, different `sre_session_id`.
3. Close tab A, reopen URL → empty chat, new ID.
4. DevTools → Cookies: no `session_id` cookie.
5. Edge: `sessionStorage.setItem('sre_session_id','not-a-uuid')` + reload → page stays empty (restore bails on 400); sending a chat shows "Request failed".

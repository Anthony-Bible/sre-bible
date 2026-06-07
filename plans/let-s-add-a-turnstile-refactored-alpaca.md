# Plan: Add Cloudflare Turnstile to sre.bible

## Context

The site exposes one user-driven write path — `POST /chat` — which both drives the (expensive) RAG/Anthropic pipeline and is the vehicle for the `send_contact_email` LLM tool. There is **no traditional contact form**; the email is sent server-side as a tool call mid-stream. That makes `/chat` the single chokepoint where bot abuse (LLM-cost burning, contact spam) must be stopped.

We will gate `/chat` with a **Cloudflare Turnstile (managed widget)**.
- Site key (public): `0x4AAAAAADgYcZEOr5G-BmBi`
- Managed mode is configured in the Cloudflare dashboard; the HTML embed is identical regardless of mode.

**Decisions (confirmed with user):**
- **Verify once per session** — the first message of a session requires a valid token; once verified, the session is marked trusted in the DB and subsequent messages skip verification. (Turnstile tokens are single-use / ~5-min TTL, so per-message gating fights the multi-turn chat UX.)
- **Always required** — there is no fail-open. Missing `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_KEY` is a fatal startup error, mirroring `ANTHROPIC_API_KEY`. Local dev uses Cloudflare's documented test keys.

## Approach

### 1. New package: `internal/turnstile`
Create `internal/turnstile/turnstile.go` — the **first** outbound `net/http` client in this codebase, so it sets the idiomatic pattern:
- `type Verifier struct { secret string; client *http.Client; log *slog.Logger }`
- `func NewVerifier(secret string, log *slog.Logger) *Verifier` — `http.Client{Timeout: 10 * time.Second}`.
- `func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) (bool, error)`:
  - `http.NewRequestWithContext(ctx, POST, "https://challenges.cloudflare.com/turnstile/v0/siteverify", ...)` with `application/x-www-form-urlencoded` body `secret`, `response=token`, optional `remoteip`.
  - `defer resp.Body.Close()`, decode `{ "success": bool, "error-codes": []string }`, wrap errors with `fmt.Errorf("...: %w", err)`, log failures with `slog`.
  - Return `success`. A network/decode failure returns `(false, err)` so the caller fails closed.

Follows `internal/email/ses.go` as the structural analog (small adapter struct + single method + context threading).

### 2. Session "verified" state (DB)
- New migration `internal/db/migrations/0005_add_turnstile_verified.sql` (goose up/down): `ALTER TABLE sessions ADD COLUMN turnstile_verified BOOLEAN NOT NULL DEFAULT false;`
- Add to `internal/db/session.go` (`*SessionStore`):
  - `IsSessionVerified(ctx, sessionID) (bool, error)` — `SELECT turnstile_verified FROM sessions WHERE id=$1`; returns `false` when no row (pgx `ErrNoRows`).
  - `MarkSessionVerified(ctx, sessionID) error` — `UPDATE sessions SET turnstile_verified = true WHERE id=$1`.
- Extend the `SessionRepository` interface in `internal/server/server.go` with both methods (interface defined where consumed).

### 3. Gate `POST /chat` (`internal/server/handlers.go`)
In `handleChat`, after `requireSession` + `ParseForm` and after the existing idempotent `CreateSession(ctx, sid)`:
1. `verified, err := s.sessions.IsSessionVerified(ctx, sid)` (handle err → SSE error).
2. If `!verified`:
   - `token := strings.TrimSpace(r.FormValue("cf-turnstile-response"))` — empty → reject (`http.StatusForbidden` *before* SSE headers are written, with a clear "verification required" body).
   - `remoteIP := r.Header.Get("CF-Connecting-IP")` (fallback to `r.RemoteAddr`) — Cloudflare-proxied per ADR 0004.
   - `ok, err := s.turnstile.Verify(ctx, token, remoteIP)`; on `!ok` or `err` → `http.StatusForbidden`, "verification failed". Fail closed.
   - On success → `s.sessions.MarkSessionVerified(ctx, sid)` (log-and-continue on error; do not block the answer).
3. Continue with the existing flow unchanged.

Order the rejection branches **before** the `text/event-stream` headers are set so a refusal is a clean 403, not a mid-stream SSE error.

### 4. Server wiring (`internal/server/server.go`)
- Add a consumer-side interface `type TurnstileVerifier interface { Verify(ctx context.Context, token, remoteIP string) (bool, error) }`.
- Add `turnstile TurnstileVerifier` and `turnstileSiteKey string` to the `Server` struct.
- Extend `NewServer(...)` to accept the verifier and site key (update the single call site in `cmd/server/main.go`).
- `chatData` (`handlers.go:41`) gains `TurnstileSiteKey string`; `handleIndex` sets it from `s.turnstileSiteKey`.

### 5. Startup config (`cmd/server/main.go`)
In `run()`, alongside the other required keys (~line 61):
```go
turnstileSiteKey := os.Getenv("TURNSTILE_SITE_KEY")
if turnstileSiteKey == "" { return fmt.Errorf("TURNSTILE_SITE_KEY is required") }
turnstileSecret := os.Getenv("TURNSTILE_SECRET_KEY")
if turnstileSecret == "" { return fmt.Errorf("TURNSTILE_SECRET_KEY is required") }
```
Build `turnstile.NewVerifier(turnstileSecret, log)` and pass it + site key into `server.NewServer(...)`.

### 6. Frontend (`internal/server/templates/index.html`)
- Head (~line 8): `<script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>`.
- Inside the chat `<form>` (~line 256, near the Send button): `<div class="cf-turnstile" data-sitekey="{{.TurnstileSiteKey}}"></div>` (implicit render; managed mode comes from the dashboard).
- In `handleSubmit` (~line 408), read the token and add it to the POST body:
  ```js
  const params = new URLSearchParams({ question: q });
  const tt = window.turnstile && turnstile.getResponse();
  if (tt) params.append('cf-turnstile-response', tt);
  // ...body: params
  ```
  After a 403 from the server, surface a friendly "Please complete the verification challenge" via the existing `showError(...)`. Subsequent messages need no token (server skips verified sessions).

### 7. Deployment (`deploy/`)
- `deploy/deployment.yaml`: add `TURNSTILE_SITE_KEY` as a plain `value: "0x4AAAAAADgYcZEOr5G-BmBi"` (public, like `AWS_REGION`), and `TURNSTILE_SECRET_KEY` via `secretKeyRef` → `sre-bible-secrets`.
- The `sre-bible-secrets` Secret is created out-of-band; add `--from-literal=TURNSTILE_SECRET_KEY=<key>` to the documented `kubectl create secret` command in `deploy/README.md` (~lines 89-100) and note it must be applied before ArgoCD syncs the new env ref.

## Critical files
- **New:** `internal/turnstile/turnstile.go`, `internal/turnstile/turnstile_test.go`, `internal/db/migrations/0005_add_turnstile_verified.sql`
- **Edit:** `internal/db/session.go`, `internal/server/server.go`, `internal/server/handlers.go`, `internal/server/templates/index.html`, `cmd/server/main.go`, `deploy/deployment.yaml`, `deploy/README.md`

## Tests (contract-focused)
- `internal/turnstile/turnstile_test.go`: spin up `httptest.Server`, point the verifier's URL at it (make the endpoint a struct field defaulting to the real URL so tests can override), assert `Verify` returns `true` on `{"success":true}`, `false` on `{"success":false,...}`, and `(false, err)` on non-200/garbage/timeout.
- `internal/server` handler tests (extend existing fakes — check `internal/server/*_test.go` for the fake `SessionRepository` pattern): add a fake `TurnstileVerifier`.
  - First message, no token → `403`, pipeline not invoked.
  - First message, verifier returns false → `403`.
  - First message, verifier returns true → pipeline runs, `MarkSessionVerified` called.
  - Already-verified session → no token needed, verifier not called.
- `internal/db/session_test.go`: if a test DB harness exists, cover `IsSessionVerified` default-false and `MarkSessionVerified` round-trip.

## Verification (end-to-end)
1. `go build ./...` and `go test ./...` (check LSP diagnostics after edits).
2. Local run — export Cloudflare **test keys** (always-pass): `TURNSTILE_SITE_KEY=1x00000000000000000000AA`, `TURNSTILE_SECRET_KEY=1x0000000000000000000000000000000AA`, plus the existing required env (`DATABASE_URL`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`); `docker-compose up` for Postgres. Confirm the server refuses to start when either Turnstile var is unset.
3. Browser (Claude-in-Chrome MCP): load the page, confirm the widget renders, send a first message and confirm it streams a reply (test key auto-passes); inspect the `/chat` request payload contains `cf-turnstile-response`. Use the always-fail test secret `2x0000000000000000000000000000000AA` to confirm a `403` + friendly error.
4. Verify a second message in the same session succeeds without re-challenging (DB `turnstile_verified = true`).

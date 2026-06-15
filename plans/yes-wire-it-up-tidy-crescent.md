# Consolidate per-message session-row reads into one SELECT

## Context

Every `POST /chat` turn reads the **same single `sessions` row three separate times**, each
fetching one column, each its own pool checkout + network round-trip:

| Call site (`internal/server/handlers.go`) | Method | Query |
|---|---|---|
| `resolvePersonaMode` (L90) | `IsDeadpoolMode` | `SELECT deadpool_mode FROM sessions WHERE id=$1` |
| `verifyTurnstile` (L311) | `IsSessionVerified` | `SELECT turnstile_verified FROM sessions WHERE id=$1` |
| `resolveInterviewMode` (L128) | `IsInterviewActive` | `SELECT interview_active FROM sessions WHERE id=$1` |

Interview mode adds a 4th read (`GetInterviewState`, `streamAnswer` L451). On a `db-f1-micro`
Cloud SQL box with a **5-connection pool**, these redundant checkouts are the bulk of the
"lots of db activity per message" the user observed. All read columns live in one row and
collapse into a single `SELECT`.

**Outcome:** normal `/chat` drops from **3 session reads → 1**; interview `/chat` from **4 → 2**.
Pure round-trip reduction with **zero semantic change** — the snapshot captures the exact same
column values the three separate reads would (we only ever write *different* columns between them
within a turn, so there is no read-after-our-own-write hazard).

Confirmed during exploration: `rag.InterviewStateStore` is **defined-but-unused** (only the
compile-time assertion in `cmd/server/main.go:43` references it). The pipeline never reads/writes
interview state during `Answer` — `streamAnswer` is the sole driver — so a pre-loaded snapshot is
safe and there is no hidden pipeline DB activity to account for.

## Approach

Add one batched read method, take the snapshot **once** at the top of `handleChat`, and pass the
relevant value into each gate helper instead of each helper re-querying. Keep the individual
methods on the store — they are still used by `/suggestions` (`IsSessionVerified`), `streamAnswer`
(`GetInterviewState`), the DB contract tests, and the `InterviewStateStore` assertion. This is
purely **additive** to the interface; minimal blast radius.

### 1. New snapshot type + interface method — `internal/server/server.go`

Define alongside `StoredMessage`:

```go
// SessionState is a one-shot snapshot of the per-session flags read on every chat turn,
// fetched in a single SELECT to avoid three separate single-column reads of the same row.
// A missing session yields the zero value (all false / nil), mirroring the per-method
// "unknown session → default" contract of IsDeadpoolMode / IsSessionVerified / IsInterviewActive.
type SessionState struct {
	Verified        bool
	DeadpoolMode    bool
	InterviewActive bool
	InterviewState  *rag.InterviewState
}
```

Add to the `SessionRepository` interface (server.go:49):

```go
GetSessionState(ctx context.Context, sessionID string) (SessionState, error)
```

### 2. Implement the batched read — `internal/db/session.go`

`db/session.go` already imports `internal/server`, so it can return `server.SessionState`
directly (same pattern as `ListMessages` returning `[]server.StoredMessage`). Mirror the
`pgx.ErrNoRows → zero value` and interview-state JSONB unmarshal already in
`IsDeadpoolMode` / `GetInterviewState`:

```go
func (s *SessionStore) GetSessionState(ctx context.Context, sessionID string) (server.SessionState, error) {
	var st server.SessionState
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT turnstile_verified, deadpool_mode, interview_active, interview_state
		 FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&st.Verified, &st.DeadpoolMode, &st.InterviewActive, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return server.SessionState{}, nil
	}
	if err != nil {
		return server.SessionState{}, fmt.Errorf("get session state: %w", err)
	}
	if len(raw) > 0 {
		var is rag.InterviewState
		if err := json.Unmarshal(raw, &is); err != nil {
			return server.SessionState{}, fmt.Errorf("unmarshal interview state: %w", err)
		}
		st.InterviewState = &is
	}
	return st, nil
}
```

### 3. Thread the snapshot through the handlers — `internal/server/handlers.go`

Change the three gate helpers to **accept** the pre-read value instead of querying:

- `resolvePersonaMode(ctx, sid, r, isDeadpool bool) context.Context` — drop the `IsDeadpoolMode`
  call; use `isDeadpool` as the starting value. Keep the `SetDeadpoolMode` write-avoidance logic
  unchanged (this is what `TestResolvePersonaMode_Optimization` pins).
- `verifyTurnstile(ctx, w, r, sid, verified bool) bool` — drop the `IsSessionVerified` call (and
  its 500-on-read-error branch, relocated to step below); short-circuit `if verified { return true }`.
- `resolveInterviewMode(ctx, sid, r, active bool) context.Context` — drop the `IsInterviewActive`
  call; use `active`.

`handleChat` (read once, preserve current ordering — `resolvePersonaMode` still runs before
`CreateSession`, so read the snapshot before it):

```go
state, err := s.sessions.GetSessionState(ctx, sid)
if err != nil {
	s.log.ErrorContext(ctx, "get session state", slog.Any("err", err), slog.String("session", sid))
	if s.turnstile != nil {
		// Can't confirm verification → must not bypass the gate. Mirrors the old
		// verifyTurnstile 500-on-read-error behavior.
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	state = SessionState{} // no turnstile (tests/local): degrade to defaults, as the old
	                        // deadpool/interview reads did (logged, non-fatal).
}
ctx := s.resolvePersonaMode(r.Context(), sid, r, state.DeadpoolMode)
// ... CreateSession ...
if !s.verifyTurnstile(ctx, w, r, sid, state.Verified) { return }
// ... chatLimiter ...
ctx = s.resolveInterviewMode(ctx, sid, r, state.InterviewActive)
```

`handleMessages` (handlers.go:182): read the snapshot (still one round-trip, replacing the lone
`IsDeadpoolMode`) and pass `state.DeadpoolMode`. No turnstile gate here, so a read error is logged
and degrades to the zero value (non-fatal, matching today).

**Leave `streamAnswer`'s `GetInterviewState` (L451) as-is.** It must read *after*
`resolveInterviewMode` may have just seeded a fresh state (reset / first flip-on), so it cannot use
the pre-`CreateSession` snapshot without reintroducing a staleness bug in the HUD-progress persist
(`graded > gradedBefore && interviewState != nil`). Keeping it means interview turns do 2 reads
instead of 1 — an acceptable, low-risk stopping point for the rare path. (Collapsing interview to a
single read would require `resolveInterviewMode` to return the seeded state; deliberately deferred.)

**Leave `handleSuggestions` (L252) as-is** — it is a separate endpoint doing a single
`IsSessionVerified` read, not the per-message hot path.

### 4. `cmd/server/main.go`

No edit needed — the assertion `_ server.SessionRepository = (*db.SessionStore)(nil)` (L35) now
also enforces the new method; it compiles once step 2 lands. Build to confirm.

## Tests

- **`internal/server/handlers_test.go`** — add to `stubSessions`: a `getStateErr error` field and

  ```go
  func (s *stubSessions) GetSessionState(_ context.Context, _ string) (SessionState, error) {
  	return SessionState{
  		Verified:        s.isVerified,
  		DeadpoolMode:    s.deadpoolMode,
  		InterviewActive: s.interviewActive,
  		InterviewState:  s.interviewState,
  	}, s.getStateErr
  }
  ```

  Sourcing from the existing fields means `TestResolvePersonaMode_Optimization`, the Turnstile
  tests, and the interview tests pass **unchanged** (they set `deadpoolMode` / `isVerified` /
  `interviewActive` / `interviewState` fields and assert on the write contract).
- **`internal/db/session_test.go`** — add `TestGetSessionState_RoundTrip` (using the existing
  `testSessionDB` / `sessionID` helpers): seed a session, set verified + deadpool + interview state,
  assert `GetSessionState` returns all four fields; and `TestGetSessionState_MissingSession` asserts
  the zero value + nil error for an unknown id.

## Verification

```bash
# Unit tests (no DB) — handler/turnstile/interview/optimization stubs
make test-unit

# New DB contract test
TEST_DATABASE_URL=postgres://sre:sre@localhost:5432/sre_bible?sslmode=disable \
  go test ./internal/db/... -run GetSessionState -v -count=1

# Lint + full suite
make lint
make db-up && make migrate
TEST_DATABASE_URL=postgres://sre:sre@localhost:5432/sre_bible?sslmode=disable \
  go test ./... -count=1
```

Optional manual confirmation: `make serve`, send a chat turn, and confirm Postgres
`log_statement=all` (or the metrics) show **one** `SELECT ... FROM sessions` per turn instead of three.

## Out of scope (noted, not done)

- Collapsing the interview path to a single read (would thread the seeded state out of
  `resolveInterviewMode`).
- Removing the dead `rag.InterviewStateStore` interface.
- `/suggestions`' single `IsSessionVerified` read.

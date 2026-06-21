# ADR 0014 — A session cache is not a spike defense; harden the spike vectors instead

## Status

Accepted

## Context

This work began as GitHub issue #62 — phase 2 of the session-state DB-load
reduction (ADR 0013): extend the galaxycache tier from the single monotonic
`verified` bit to versioned-key full-`SessionState` caching on the `/chat` hot
path, keyed by a `{uuid}:{gen}` token.

Two things surfaced while grilling that plan:

1. **galaxycache can't do what #62 asked.** galaxycache is read-through *only* —
   the getter caches the value under the *exact requested key*
   (`g.populateCache(ctx, key, value)`); there is no `Set`/`Put`/warm API.
   "Cache under the true generation" is not directly implementable without
   carrying the generation in a client-round-tripped token, and the session id
   arrives via `X-Session-Id` / `sessionStorage` with no client-carried version
   to key on (the same gap recorded at the end of ADR 0013).

2. **The real concern was different.** The trigger was a planned **Hacker News**
   post and the fear that the Cloud SQL `db-f1-micro` (ADR 0004 — 5-connection
   pool per replica, 2 replicas) would topple. The question actually being asked
   was "should we replace the cache with Redis to survive the spike?"

**The controlling insight that re-scoped this work:** *a session cache is not a
spike defense.* An HN front-page spike is a flood of **unique new session
UUIDs** — every one is a guaranteed cache **miss** that falls through to the DB.
A cache (galaxycache *or* Redis) only accelerates *repeat* reads of the *same*
key; it does nothing for a thundering herd of first-time visitors. Redis would
add a second stateful service that is itself spike-vulnerable, a new network
dependency, and a new failure mode — all to accelerate reads that, in a spike,
do not repeat.

## Decision

**Harden the actual spike-toppling vectors rather than add or extend a session
cache.** Concretely, three layers, in order of leverage:

1. **Cut per-visitor DB work (the biggest, cheapest win).** `restoreHistory()`
   in `templates/index.html` fired `GET /messages` unconditionally on
   `DOMContentLoaded`, costing two DB reads (`GetSessionState` + `ListMessages`)
   for every first-time visitor — who has *no* history to restore. A new
   non-creating `hasStoredSession()` peek (distinct from `getSessionId()`, which
   lazily *mints* an id) gates the fetch: a brand-new tab has no
   `sre_session_id` in `sessionStorage` (it is created only by the
   post-interaction `/chat` and `/suggestions` paths), so "no stored id" is a
   reliable proxy for "nothing on the server to restore." The common spike
   visitor (load, read, leave) now costs **0 DB reads**; only visitors who have
   actually chatted pay for `/messages` on reload.

2. **Fail fast instead of piling up when the 5-connection pool saturates.** Two
   complementary bounds:
   - **DB-side backstop (`internal/db/db.go`):**
     `cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"` caps any single
     SQL statement at ~5s so a slow/stuck query cannot hold a connection forever
     and starve the other four. (Set before the bootstrap `ConnConfig.Copy()` so
     the bootstrap conn inherits it. **Caveat, documented at the call site:**
     goose migrations run on this same pool, so the cap also applies to migration
     DDL — current migrations are tiny idempotent ops and are safe, but any future
     heavy DDL, e.g. building an ivfflat/hnsw index on a populated table, must
     override locally with `SET LOCAL statement_timeout = 0;`.)
   - **Request-side load-shed (`internal/server/handlers.go`):** a short-deadline
     "quick DB" context (`quickDBTimeout`, default 2500ms, env-overridable via
     `DB_QUICK_TIMEOUT_MS`) wraps the *pre-stream* DB ops only.
     `pgxpool.Acquire` honours the context deadline, so a saturated pool **sheds
     to HTTP 503 + `Retry-After`** instead of queuing goroutines and ballooning
     latency. The deadline (2.5s) is deliberately shorter than `statement_timeout`
     (5s) so the acquire-wait fires first. In `handleChat` the `ListMessages` read
     is moved *before* the SSE header block so a shed is a clean 503, never a
     `200` with an SSE error frame; the long-running LLM stream keeps the full
     `r.Context()`. `verifyTurnstile` also stays on the long context — it is
     primarily an external Cloudflare HTTP verify, and a 2.5s DB deadline there
     would risk false `403`s under network latency (its only DB write,
     `MarkSessionVerified`, is already best-effort/non-fatal, and `CreateSession`
     runs first under the quick deadline, so the pool already sheds before
     turnstile is reached). A new `sre_bible_db_load_shed_total` counter, labelled
     by `endpoint` (`messages`/`suggestions`/`chat`), records each shed.

3. **Serve the static shell from the edge, and scale the DB, not the app (ops;
   no code).** `GET /` is a DB-free template render identical for all anon
   visitors. Front the zone with Cloudflare and cache `GET /` with a short edge
   TTL + `stale-while-revalidate` so origin sees ~1 landing-page request/min
   regardless of crowd size (never cache `/messages`/`/chat`/`/suggestions`/
   `/healthz`/`/readyz`). For the launch window, temporarily bump `db_tier`
   (`deploy/terraform/variables.tf`) above `db-f1-micro`. **Do not** raise replica
   count — more replicas × `MaxConns = 5` = more connections against the tiny DB,
   making saturation *worse*. Scale the DB, not the app, for this workload.

## Consequences

- **#62 is deferred** (not rejected). A session cache does not defend against a
  unique-new-session spike; revisit only if profiling later shows *repeat*
  same-session reads dominating steady-state DB load. Phase-1 cache
  (`internal/cache/`, ADR 0013) stays as-is and is untouched.
- **Redis is rejected** for this goal — a second spike-vulnerable stateful
  service that does not address unique-key misses.
- A saturated pool now returns **503 + `Retry-After`** quickly (≈ the quick-DB
  timeout) so clients back off, rather than hanging while goroutines and latency
  balloon. `/suggestions`, which otherwise degrades silently to
  `{"questions":[]}`, returns an explicit 503 on saturation so the client backs
  off instead of silently showing no cards.
- First-time visitors cost **0 DB reads** on page load; the per-visitor DB floor
  drops to what their interactions actually require.
- A new bounded-cardinality metric (`sre_bible_db_load_shed_total{endpoint}`) and
  one new optional env var (`DB_QUICK_TIMEOUT_MS`).
- The edge-cache and DB-tier-bump steps are ops actions validated out-of-band at
  launch, not gated by code tests.

## Alternatives considered

- **Extend the cache to full `SessionState` (#62) / add Redis.** Both accelerate
  *repeat* reads of the *same* key. A spike is the opposite workload — unique
  first-time keys, all misses — so neither defends the DB against the thing we are
  actually afraid of, and Redis additionally introduces a second spike-vulnerable
  service. Rejected for this goal; #62 deferred.
- **Autoscale the replica count.** Each replica holds its own 5-connection pool,
  so more replicas multiply connections against a `db-f1-micro` with a very low
  `max_connections` and shared CPU — saturation gets *worse*. Rejected; scale the
  DB instead.
- **`statement_timeout` alone (no request-side shed).** `statement_timeout`
  bounds a query only *after* a connection is acquired; it does nothing for time
  spent *waiting* for a free connection — which is exactly the pile-up vector.
  The short request context is needed to bound the acquire-wait. Both are kept:
  no stuck connections (B1) and no unbounded queue (B2).
- **Silent degrade on `/suggestions` under saturation.** The endpoint already
  degrades to `{"questions":[]}` + `200` for non-abuse failures. For pool
  saturation an explicit `503` is preferred so the client backs off rather than
  silently rendering no follow-up cards and immediately retrying.

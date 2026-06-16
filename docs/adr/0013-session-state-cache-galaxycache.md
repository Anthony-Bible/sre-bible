# ADR 0013 — Distributed Session-State Cache with galaxycache (phase 1: the `verified` flag)

## Status

Accepted

## Context

Every `POST /chat`, `GET /messages`, and `POST /suggestions` opens a DB round-trip for
per-session state *before* any rate-limit check, embedding, or LLM call. The backing store is a
Cloud SQL `db-f1-micro` with a 5-connection pool per replica (2 replicas, ADR 0004). Under a
burst — a job application goes out and a cohort of recruiters hits the app within minutes —
these unconditional reads compete for a tight connection budget to return flags that almost
never change.

The `turnstile_verified` flag is the cleanest possible cache target: it is **monotonic**. Its
only writer anywhere in the repo is `MarkSessionVerified` (`UPDATE … = true`,
`internal/db/session.go`); there is no reset path. Once a session is verified it stays verified
for its lifetime.

This is **phase 1 of a two-phase program**. Phase 1 stands up a distributed, fail-open cache
tier behind `server.SessionRepository` and removes the DB read on the `/suggestions`
verified-gate (`IsSessionVerified`). Phase 2 (#62) extends the same tier to versioned-key
full-`SessionState` caching for the `/chat` hot path.

**Honest scoping.** `/chat` and `/messages` read `verified` via `GetSessionState`, a single
SELECT that *also* returns the mutable `deadpool_mode` / `interview_active` / `interview_state`
fields needed every turn — so phase 1 cannot remove the `/chat` SELECT, and `GetSessionState`
stays a pure pass-through. Phase 1's measurable saving is on `POST /suggestions`, which calls
the verified-only `IsSessionVerified`. A per-replica in-process set would give a near-identical
DB-load profile for two replicas; galaxycache + peer-to-peer is chosen **deliberately** as the
phase-2 foundation and as a demonstration of the pattern done right, not because phase-1 load
math requires it.

## Decision

Add a new `internal/cache` package: a transparent **decorator** (`CachingSessionStore`) that
implements all twelve `server.SessionRepository` methods. Eleven pass straight through to the
wrapped `*db.SessionStore`; only `IsSessionVerified` consults a [galaxycache](https://github.com/vimeo/galaxycache)
galaxy named `session-verified`. A `Tier` type owns the galaxycache `Universe`/`Galaxy`
lifecycle: the decorator (`Store()`), the cluster-internal peer HTTP endpoint (`Handler()`, the
**HTTP** fetch protocol so gRPC is not promoted to a direct dependency), the DNS-driven peer
refresh loop (`RefreshPeers`), the OTel metrics bridge (`RegisterMetrics`), and `Close`.

The whole tier is gated behind `SESSION_CACHE_ENABLED` (default **false**). When disabled, the
decorator, the `:9091` listener, and the refresh goroutine are never constructed — behaviour is
bit-for-bit unchanged.

**The DB read lives in the galaxycache backend getter, not the decorator.** galaxycache is
read-through *only* — there is no `Set`/warm API — so the getter is the single place a DB read
can happen on a miss.

**Trust-true / bypass-false — never cache `false` (option A).** The getter models the
*verified-set*:

```go
v, err := store.IsSessionVerified(ctx, key)
if err != nil { return fmt.Errorf("…: %w", err) }   // wrapped: not cached, propagates
if !v        { return galaxycache.TrivialNotFoundErr{} }   // NotFound: not cached
return dest.UnmarshalBinary([]byte{1})              // cache a 1-byte "true" sentinel
```

galaxycache skips caching on **any** getter error, so `false` is never cached and a later
`false→true` flip is observed on the very next lookup — **no TTL in the correctness path**.
Returning `NotFound` (rather than a generic error) additionally suppresses cross-peer local
fall-through, so the unverified cross-replica path costs one DB read, not two.

The decorator maps the galaxy result:

```
galaxy.Get(...) == nil                  → (true,  nil)   result="verified"    DB read avoided
errors.As(err, &NotFoundErr)            → (false, nil)   result="unverified"  DB read happened
otherwise                               → (false, err)   result="error"       gate FAILS CLOSED
```

**Fail-open at the tier, fail-closed at the gate.** On a backend error the error propagates
unchanged so `/chat`'s and `/suggestions`' gates still refuse rather than assuming
`verified=true`. **Correctness does not depend on `NotFound` (or any value) surviving the HTTP
hop:** a cold cache, an evicted entry, or a lost peer can only ever cause an *extra DB read*,
never a wrong answer. This is what makes the feature safe to ship behind a flag and safe to fail.

**TTL is eviction hygiene only.** `SESSION_CACHE_TTL_SECONDS` is wired via galaxycache's
`WithGetTTL(ttl, ttl/10)` (~10% jitter to avoid synchronised expiry). An evicted `true` is
simply re-fetched and re-cached; a stale `true` is still correct because the flag is monotonic.

**Topology.** Peers are discovered by resolving a new headless Service
(`sre-bible-headless`, `clusterIP: None`) every `SESSION_CACHE_PEER_REFRESH_SECONDS` and calling
`universe.Set(http://<podIP>:9091, …)`. The self URL (`http://$MY_POD_IP:9091`) is always
included so a replica stays authoritative for its own hash range even before the Service reports
it ready. `MY_POD_IP` comes from the Downward API and is fatal at startup only when the cache is
enabled. The `:9091` endpoint is cluster-internal (no ingress).

## Consequences

- **New direct dependency** on `github.com/vimeo/galaxycache` (HTTP fetch protocol only; gRPC
  stays transitive-free).
- **A third HTTP listener** (`:9091`) and a peer-refresh goroutine exist **only when enabled**;
  the 30 s refresh fits inside `terminationGracePeriodSeconds: 40`. The new headless Service is
  inert (selects the same pods) until the cache is switched on.
- **New observability:** a synchronous `sre_bible_session_cache_lookups{result}` counter, plus
  four observable counters bridged from galaxycache's `GalaxyStats`
  (`…_maincache_hits` = DB reads avoided, `…_backend_loads`, `…_peer_loads`,
  `…_backend_load_errors`), all bounded to the single `galaxy="session-verified"` label.
- **No DB migration**; no schema change.
- **The saving is bounded to `/suggestions` in phase 1.** This is recorded honestly above — the
  value is as much the phase-2 foundation and the demonstration as the immediate load relief.

## Alternatives considered

- **Redis / Memorystore.** A managed cache would centralise the state and remove the peer-mesh
  entirely. Rejected for phase 1: it adds a new managed service, a new network dependency, and a
  new failure mode and cost line, to cache a single monotonic bit. galaxycache keeps the cache
  in-process and peer-to-peer over plain HTTP, idiomatic to a Go service, with no new
  infrastructure to operate.
- **A per-replica in-process set (e.g. `sync.Map`).** For two replicas this is *load-equivalent*
  to galaxycache (each replica would still miss on sessions it hasn't seen and read the DB once).
  It is simpler. galaxycache was chosen **deliberately** because phase 2 needs the distributed,
  consistent-hashed, versioned-key substrate, and standing it up now — behind a kill-switch, on
  the trivially-safe monotonic flag — de-risks phase 2 and demonstrates the pattern end to end.
- **Caching `false` with a short TTL.** Rejected: it puts correctness on a TTL timer (a
  `false→true` flip would be invisible until expiry) for no benefit, since the unverified path is
  exactly the one we *want* to keep reading the DB until it flips. Never caching `false` makes the
  flip latency zero and the cache correctness TTL-independent.
- **Warming the cache from `GetSessionState` / a decorator fall-through to the store.** Infeasible:
  galaxycache has no write/warm API, so the only place a DB read can occur is the read-through
  backend getter. The original issue's "decorator falls through to `store.IsSessionVerified`" and
  "warm the cache in `GetSessionState`" sketches were dropped for this reason.
- **Version-oracle key-versioning now (phase-2 context).** Phase 2 wants a cache key that encodes
  a state version so a write invalidates by bumping the version. The usual trick — version the key
  from a cookie/header the client already round-trips — is **inapplicable here**: the session
  identifier arrives via `X-Session-Id` / `sessionStorage`, and there is no client-carried version
  token to key on. Phase 2 will therefore need a server-derived version oracle (e.g. an
  `updated_at`/sequence column), which is out of scope for the monotonic phase-1 flag and recorded
  here so the phase-2 design starts from the right premise.

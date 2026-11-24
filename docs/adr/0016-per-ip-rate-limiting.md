# ADR 0016 — Per-IP rate limiting as the abuse backstop behind session-ID rotation

## Status

Accepted

## Context

This work is GitHub issue #53, a follow-up to the rate-limiting work in ADR-less
PRs #51 (the reusable `internal/ratelimit` package + `POST /suggestions`) and #54
(`POST /chat`, issue #52). Both endpoints key their limiter on the
client-supplied session ID (`X-Session-Id`, a UUID v4).

The security review of #51 flagged that a session-keyed cooldown is **rotatable**:
`X-Session-Id` is minted client-side and costs nothing to regenerate, so an
attacker issues a fresh UUID per request and defeats the per-session cooldown
entirely. What remains is the per-replica global hourly cap (a blunt
all-visitors-share-it backstop) and the once-per-session Turnstile gate. A
per-IP dimension instead targets the actual offending **host**, which an attacker
cannot rotate for free.

The `ratelimit.Limiter` is key-agnostic, so mechanically this is "call `Allow(ip)`
as well as `Allow(sid)`." The substance is **policy and trust**, not the limiter:

1. **Trustworthy client IP (the hard prerequisite).** `r.RemoteAddr` behind
   Cloudflare is the CF edge IP — shared across all visitors, so every visitor
   would land in one bucket. The real client IP is `Cf-Connecting-Ip`, but that
   header is client-supplied and only trustworthy if the GCP origin is reachable
   **only** via Cloudflare. If the origin were directly reachable, an attacker
   could forge `Cf-Connecting-Ip` to bypass the limit, or spoof a victim's IP to
   exhaust *their* bucket — a targeted DoS the session-keyed version can't do.
   **The origin is confirmed Cloudflare-locked**, so `Cf-Connecting-Ip` is treated
   as authoritative. This ADR records that dependency; if the origin lockdown is
   ever removed, this per-IP limiting must be disabled (it becomes weaker than the
   session-keyed design it augments).

2. **Shared-egress collateral.** Corporate NAT, VPNs, universities and mobile
   CGNAT put many legitimate users behind one IP. The audience here is recruiters,
   who plausibly share a company NAT. Per-IP throttling penalizes them as a group;
   this is inherent to per-IP limiting and is mitigated (not removed) by keeping
   the per-session dimension for the common case.

## Decision

**Augment, don't replace.** Keep the per-session cooldown (fairness/pacing among
verified humans; no shared-NAT problem) and add an independent per-IP dimension as
the abuse backstop, on both `/chat` and `/suggestions`, each with its own budget.

- **Key derivation (`internal/server/clientip.go`).** `clientIPKey` reads
  `Cf-Connecting-Ip` (falling back to `r.RemoteAddr` for local/direct dev) and
  normalizes:
  - IPv4 (and IPv4-mapped IPv6, unmapped) → the address as-is.
  - IPv6 → the **/64 prefix**, not the raw /128. A subscriber is routinely given a
    whole /64; an attacker with one /64 owns billions of distinct /128s. Keying on
    /64 throttles the entity that maps to a subscriber and denies the per-/128
    bypass.
  - Unparseable → `""`, and the handler **fails open** to the per-session check
    (no break for local dev; behind the CF-locked origin the header is always a
    valid IP). `verifyTurnstile` was refactored to share the same `clientIP`
    extraction it already did inline.

- **Burst-capable limiter (`internal/ratelimit`).** The existing limiter's per-key
  dimension is a strict one-token-per-interval (burst 1). For a *generous* per-IP
  hourly budget that still absorbs a shared NAT's concurrent legitimate users, a
  new `NewWithBurst(perKey, perKeyBurst, globalPerHour)` adds a configurable
  per-key burst (`New` delegates with burst 1, unchanged). A per-IP limiter is
  `NewWithBurst(time.Hour/perHour, 20, 0)`: refills `perHour` tokens/hour, bursts
  20, with the process-global dimension disabled (the per-session limiters already
  own the global cap). The idle-eviction TTL was changed from `perKey` to
  `perKey*perKeyBurst` — the full refill window — so an evicted-then-recreated key
  can never be handed a fresh full burst early (which would defeat the hourly cap).

- **Gate placement (`internal/server/handlers.go`).** The per-IP check runs
  **before** the per-session `Allow(sid)` on both endpoints, so a per-IP rejection
  doesn't consume a session token. Throttle → `429` before any embedding / DB read
  / LLM call, counted distinctly: `LLMResponsesBlocked{reason="rate_limited_ip"}`
  for `/chat`, `FollowUpSuggestions{status="throttled_ip"}` for `/suggestions`.

- **Config.** `CHAT_RATE_LIMIT_PER_IP_PER_HOUR` (default 120) and
  `FOLLOWUP_RATE_LIMIT_PER_IP_PER_HOUR` (default 240), via the existing
  `envPositiveInt` (invalid/non-positive → default + warn). Default ON, since the
  origin lockdown prerequisite is met. The burst (20) is a documented constant, not
  an env knob, to avoid config sprawl.

## Consequences

- **Privacy preserved.** The IP (or /64) is used **only** as an ephemeral in-memory
  limiter key — never logged, never persisted, never a metric label value
  (cardinality + PII). Consistent with the project's PII-free-traces posture.
- **Session-ID rotation no longer defeats the cooldown**: an attacker minting fresh
  UUIDs still hits the per-IP bucket for their host.
- **Shared-NAT collateral is accepted and mitigated**: per-IP defaults are set
  generously (120–240/hour with a burst of 20) so a company NAT of real recruiters
  isn't throttled on its first interactions; the kept per-session dimension handles
  the common case. Tune the env vars up if a legitimate shared egress is throttled.
- **Two new bounded-cardinality metric label values** (`rate_limited_ip`,
  `throttled_ip`) and two new optional env vars.
- **Per-replica, like the existing limiters** (ADR-less #51): the per-IP cap is
  per-replica with no session affinity, so aggregate ≈ replica count × cap. An
  accepted trade-off, same as the session dimension.
- **Operational dependency recorded**: correctness rests on the origin staying
  Cloudflare-locked. Removing that lockdown requires disabling per-IP limiting.

## Alternatives considered

- **Replace the session dimension with per-IP.** Rejected — the per-session
  cooldown is the fairness/pacing control for verified humans and has no shared-NAT
  problem. The two are complementary; per-IP is the backstop, not a substitute.
- **Key IPv6 on the full /128.** Rejected — an attacker owns billions of /128s in a
  single /64, so /128 keying is trivially bypassed; /64 maps to the subscriber.
- **Reuse the limiter as-is (burst 1).** Rejected — a burst-1 per-IP limiter forces
  one-request-per-interval on a whole IP, so a shared NAT of concurrent real users
  would see all-but-one throttled. The configurable burst is what makes per-IP
  limiting tolerable for the recruiter-on-company-NAT audience.
- **A dedicated DoS/IP spoofing defense at the app layer.** Out of scope — IP
  trust is an edge/origin concern (the Cloudflare lockdown), not something the Go
  process can or should re-litigate per request.

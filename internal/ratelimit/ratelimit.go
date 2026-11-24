// Package ratelimit provides a small, reusable in-process rate limiter that
// combines a per-key minimum interval with a process-wide ceiling.
//
// It is keyed by an arbitrary string — a session ID, a client IP, an API token,
// whatever the caller throttles by — so it is not tied to any one endpoint. The
// limiter is in-process only (no shared store): with multiple replicas the
// global cap applies per replica, and a key alternating replicas can halve its
// effective cooldown. That trade-off is deliberate; rejecting an over-limit
// request costs zero I/O, which is the whole point when the resource being
// protected is a small database connection pool.
//
// Built on golang.org/x/time/rate. Each bucket is a token-bucket whose state is
// pure arithmetic over timestamps — there is no background goroutine, no ticker,
// and no channel. Allow never blocks: it returns immediately so callers can
// reject (e.g. HTTP 429) rather than pace.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter enforces a per-key minimum interval plus a process-wide hourly
// ceiling. The zero value is not usable; construct one with New. All methods are
// safe for concurrent use.
type Limiter struct {
	mu          sync.Mutex
	keys        map[string]*keyEntry // per-key token buckets, created lazily
	perKey      time.Duration        // min interval between allowed calls per key; also the idle-eviction TTL
	perKeyBurst int                  // tokens a single key may spend in a burst (>=1)
	global      *rate.Limiter        // process-wide ceiling (a backstop)
	lastSweep   time.Time            // throttles the idle sweep to once per perKey
}

// keyEntry is one key's bucket plus the bookkeeping the idle sweep needs.
type keyEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// New builds a Limiter that allows at most one call per perKey interval for any
// single key, with a global ceiling of globalPerHour calls/hour across all keys.
//
// A non-positive perKey disables the per-key interval (every call passes the
// per-key check); a non-positive globalPerHour disables the global ceiling.
func New(perKey time.Duration, globalPerHour int) *Limiter {
	return NewWithBurst(perKey, 1, globalPerHour)
}

// NewWithBurst is New with a configurable per-key burst: a single key may spend
// up to perKeyBurst tokens back-to-back, after which it refills one token every
// perKey interval. Burst 1 reproduces New's strict one-per-interval behaviour.
//
// This expresses a per-key hourly budget with headroom for bursts: for an
// N-per-hour cap that still lets a shared source (e.g. a NAT'd /64 of real
// users) fire several requests at once, use perKey = time.Hour/N with a
// generous perKeyBurst. A perKeyBurst < 1 is floored to 1.
//
// A non-positive perKey disables the per-key interval (every call passes the
// per-key check); a non-positive globalPerHour disables the global ceiling.
func NewWithBurst(perKey time.Duration, perKeyBurst int, globalPerHour int) *Limiter {
	if perKeyBurst < 1 {
		perKeyBurst = 1
	}
	global := rate.NewLimiter(rate.Inf, 0)
	if globalPerHour > 0 {
		// Steady globalPerHour/hour with a burst of globalPerHour: a backstop
		// against aggregate abuse, not the primary per-key control.
		global = rate.NewLimiter(rate.Every(time.Hour/time.Duration(globalPerHour)), globalPerHour)
	}
	return &Limiter{
		keys:        make(map[string]*keyEntry),
		perKey:      perKey,
		perKeyBurst: perKeyBurst,
		global:      global,
	}
}

// Allow reports whether a call for key may proceed now, consuming one unit of
// budget if so. The per-key interval is checked first and only debits the global
// ceiling once a key clears its own cooldown, so a hammering key cannot drain the
// shared budget out from under quieter ones.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	e := l.keys[key]
	if e == nil {
		// rate.Every(perKey) refills one token per perKey; burst perKeyBurst lets a
		// key spend that many back-to-back. A non-positive perKey yields rate.Inf,
		// i.e. always allowed.
		e = &keyEntry{lim: rate.NewLimiter(rate.Every(l.perKey), l.perKeyBurst)}
		l.keys[key] = e
	}
	e.lastSeen = now

	// Reserve from the per-key bucket first (so a hammering key can't drain the
	// shared global budget), then the global bucket, but commit only if BOTH have a
	// token available now. If either rejects, cancel both reservations so a call
	// refused by one ceiling does not burn the other's budget — otherwise a
	// well-behaved key denied solely by a saturated global cap would still be forced
	// into a full perKey cooldown. Both limiters are only ever touched under l.mu, so
	// the reserve/cancel pair is race-free.
	rKey := e.lim.ReserveN(now, 1)
	if rKey.DelayFrom(now) > 0 {
		rKey.CancelAt(now)
		return false
	}
	rGlobal := l.global.ReserveN(now, 1)
	if rGlobal.DelayFrom(now) > 0 {
		rGlobal.CancelAt(now)
		rKey.CancelAt(now)
		return false
	}
	return true
}

// sweep evicts keys idle longer than the idle TTL. The TTL is perKey*perKeyBurst
// — the time an empty bucket takes to refill all perKeyBurst tokens — so an
// evicted-then-recreated key gets a full-but-not-larger budget: dropping it can
// never hand a key more tokens than keeping it would have. (A shorter TTL, e.g.
// perKey, would let a key spend its whole burst, idle just past perKey while only
// one token refills, get evicted, and return with a fresh full burst — defeating
// the hourly cap. lastSeen is bumped on every call, allowed or denied, so a
// hammering key never goes idle and is never evicted.)
//
// It is an O(n) scan over the live keys, but n is bounded to keys seen within the
// last idle-TTL window and the scan is amortized to at most once per idle TTL, so
// the cost is negligible at this scale. The worst case is a very large key space
// (e.g. keying by IP under a flood), where holding the mutex for the scan becomes
// the bottleneck; the escape hatch then is to shard the map or replace it with a
// time-ordered min-heap so eviction touches only the expired entries (O(k))
// rather than all of them. That is over-engineering at this scale, so we keep the
// flat scan.
//
// Caller must hold l.mu.
func (l *Limiter) sweep(now time.Time) {
	if l.perKey <= 0 {
		return
	}
	idleTTL := l.perKey * time.Duration(l.perKeyBurst)
	if now.Sub(l.lastSweep) < idleTTL {
		return
	}
	l.lastSweep = now
	for k, e := range l.keys {
		if now.Sub(e.lastSeen) > idleTTL {
			delete(l.keys, k)
		}
	}
}

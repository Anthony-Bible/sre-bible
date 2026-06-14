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
	mu        sync.Mutex
	keys      map[string]*keyEntry // per-key token buckets, created lazily
	perKey    time.Duration        // min interval between allowed calls per key
	global    *rate.Limiter        // process-wide ceiling (a backstop)
	idleTTL   time.Duration        // evict keys idle longer than this
	lastSweep time.Time            // throttles the idle sweep to once per perKey
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
	global := rate.NewLimiter(rate.Inf, 0)
	if globalPerHour > 0 {
		// Steady globalPerHour/hour with a burst of globalPerHour: a backstop
		// against aggregate abuse, not the primary per-key control.
		global = rate.NewLimiter(rate.Every(time.Hour/time.Duration(globalPerHour)), globalPerHour)
	}
	return &Limiter{
		keys:    make(map[string]*keyEntry),
		perKey:  perKey,
		global:  global,
		idleTTL: perKey, // once the cooldown elapses a bucket is full again — identical to a fresh one, so it's safe to drop.
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
		// rate.Every(perKey) with burst 1 ⇒ true at most once per perKey. A
		// non-positive perKey yields rate.Inf, i.e. always allowed.
		e = &keyEntry{lim: rate.NewLimiter(rate.Every(l.perKey), 1)}
		l.keys[key] = e
	}
	e.lastSeen = now

	if !e.lim.AllowN(now, 1) {
		return false
	}
	return l.global.AllowN(now, 1)
}

// sweep evicts entries idle longer than idleTTL. It is an O(n) scan over the live
// keys, but n is bounded to keys seen within the last idleTTL window and the scan
// is amortized to at most once per perKey, so the cost is negligible at this
// scale. The worst case is a very large key space (e.g. keying by IP under a
// flood), where holding the mutex for the scan becomes the bottleneck; the escape
// hatch then is to shard the map or replace it with a time-ordered min-heap so
// eviction touches only the expired entries (O(k)) rather than all of them. That
// is over-engineering for throttling by session ID, so we keep the flat scan.
//
// Caller must hold l.mu.
func (l *Limiter) sweep(now time.Time) {
	if l.idleTTL <= 0 || now.Sub(l.lastSweep) < l.perKey {
		return
	}
	l.lastSweep = now
	for k, e := range l.keys {
		if now.Sub(e.lastSeen) > l.idleTTL {
			delete(l.keys, k)
		}
	}
}

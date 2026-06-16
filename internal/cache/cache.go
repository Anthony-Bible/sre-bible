// Package cache provides a distributed, fail-open read-through cache tier for the
// monotonic per-session turnstile-verified flag, layered behind the
// server.SessionRepository port via a transparent decorator.
//
// Only IsSessionVerified is cached. The flag is monotonic — its sole writer is
// MarkSessionVerified (false→true, never reset) — so a cached "true" can never go
// stale in a way that matters. "false" is deliberately never cached, so a later
// false→true flip is observed on the very next lookup with no TTL in the
// correctness path. Every other SessionRepository method passes straight through
// to the wrapped store.
//
// The tier is fail-open: a backend (DB) error propagates unchanged so the calling
// gate still fails closed, and a cold cache or a lost peer can only ever cause an
// extra DB read — never a wrong answer. See docs/adr/0013.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	galaxycache "github.com/vimeo/galaxycache"
	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/server"
)

// CachingSessionStore decorates a server.SessionRepository, serving the monotonic
// turnstile-verified flag from a galaxycache galaxy and delegating every other
// method straight through to the wrapped store. It is constructed by Tier; callers
// obtain it via Tier.Store().
type CachingSessionStore struct {
	store  server.SessionRepository
	galaxy *galaxycache.Galaxy
	log    *slog.Logger
}

// newCachingSessionStore wraps store, routing IsSessionVerified through galaxy.
func newCachingSessionStore(store server.SessionRepository, galaxy *galaxycache.Galaxy, log *slog.Logger) *CachingSessionStore {
	if log == nil {
		log = slog.Default()
	}
	return &CachingSessionStore{store: store, galaxy: galaxy, log: log}
}

// newVerifiedGetter builds the galaxycache backend getter. This is where the DB
// read lives (galaxycache is read-through only — there is no Set/warm API). It
// models the verified-set: a verified session caches the one-byte sentinel; an
// unverified session returns NotFound so galaxycache caches nothing (never cache
// "false"); a store error is wrapped and propagated so galaxycache caches nothing
// and the caller's gate can fail closed.
func newVerifiedGetter(store server.SessionRepository) galaxycache.GetterFunc {
	return func(ctx context.Context, key string, dest galaxycache.Codec) error {
		verified, err := store.IsSessionVerified(ctx, key)
		if err != nil {
			// Wrapped, not a NotFound: galaxycache skips caching on any getter error,
			// so the failure is never cached and propagates to the decorator, which
			// fails the gate closed.
			return fmt.Errorf("session cache getter: is session verified: %w", err)
		}
		if !verified {
			// NotFound is never cached, so a later false→true flip is seen immediately.
			// It also suppresses cross-peer local fall-through (one DB read, not two, on
			// the unverified cross-replica path).
			return galaxycache.TrivialNotFoundErr{}
		}
		// Cache a one-byte sentinel. Its mere presence means "verified"; the value
		// itself is never inspected on read.
		return dest.UnmarshalBinary([]byte{1})
	}
}

// isNotFound reports whether err (or anything it wraps) is a galaxycache NotFound.
func isNotFound(err error) bool {
	var nf galaxycache.NotFoundErr
	return errors.As(err, &nf)
}

// IsSessionVerified returns the session's verified flag, served from the cache tier.
//
//	cache hit / fresh "true"  → (true, nil)   result="verified"   — DB read avoided
//	getter NotFound ("false") → (false, nil)  result="unverified" — DB read happened
//	any other error           → (false, err)  result="error"      — gate FAILS CLOSED
//
// Correctness does not depend on the NotFound surviving a cross-peer HTTP hop: a
// cold cache or lost peer merely re-derives the answer with an extra DB read.
func (c *CachingSessionStore) IsSessionVerified(ctx context.Context, sessionID string) (bool, error) {
	var dest galaxycache.ByteCodec
	err := c.galaxy.Get(ctx, sessionID, &dest)
	switch {
	case err == nil:
		metrics.M.SessionCacheLookups.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("result", "verified")))
		return true, nil
	case isNotFound(err):
		metrics.M.SessionCacheLookups.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("result", "unverified")))
		return false, nil
	default:
		// Fail-open at the tier, fail-closed at the gate: surface the error so the
		// caller (e.g. /chat's Turnstile gate) refuses rather than assuming verified.
		metrics.M.SessionCacheLookups.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("result", "error")))
		c.log.ErrorContext(ctx, "session cache lookup failed", slog.Any("err", err), slog.String("session", sessionID))
		return false, err
	}
}

// The remaining eleven methods pass straight through to the wrapped store. None
// touch the cache: MarkSessionVerified is the verified flag's only writer and the
// flag is monotonic, so no explicit invalidation is needed — an unverified miss is
// simply re-read until it flips true.

// CreateSession delegates to the wrapped store.
func (c *CachingSessionStore) CreateSession(ctx context.Context, sessionID string) error {
	return c.store.CreateSession(ctx, sessionID)
}

// ListMessages delegates to the wrapped store.
func (c *CachingSessionStore) ListMessages(ctx context.Context, sessionID string) ([]server.StoredMessage, error) {
	return c.store.ListMessages(ctx, sessionID)
}

// AppendMessage delegates to the wrapped store.
func (c *CachingSessionStore) AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string, trace []rag.TraceStep) error {
	return c.store.AppendMessage(ctx, sessionID, msg, citations, trace)
}

// MarkSessionVerified delegates to the wrapped store. It is the verified flag's
// only writer; the false→true flip is picked up on the next lookup because "false"
// is never cached, so no cache invalidation is required here.
func (c *CachingSessionStore) MarkSessionVerified(ctx context.Context, sessionID string) error {
	return c.store.MarkSessionVerified(ctx, sessionID)
}

// SetDeadpoolMode delegates to the wrapped store.
func (c *CachingSessionStore) SetDeadpoolMode(ctx context.Context, sessionID string, enabled bool) error {
	return c.store.SetDeadpoolMode(ctx, sessionID, enabled)
}

// IsDeadpoolMode delegates to the wrapped store.
func (c *CachingSessionStore) IsDeadpoolMode(ctx context.Context, sessionID string) (bool, error) {
	return c.store.IsDeadpoolMode(ctx, sessionID)
}

// GetInterviewState delegates to the wrapped store.
func (c *CachingSessionStore) GetInterviewState(ctx context.Context, sessionID string) (*rag.InterviewState, error) {
	return c.store.GetInterviewState(ctx, sessionID)
}

// SetInterviewState delegates to the wrapped store.
func (c *CachingSessionStore) SetInterviewState(ctx context.Context, sessionID string, state *rag.InterviewState) error {
	return c.store.SetInterviewState(ctx, sessionID, state)
}

// ClearInterviewState delegates to the wrapped store.
func (c *CachingSessionStore) ClearInterviewState(ctx context.Context, sessionID string) error {
	return c.store.ClearInterviewState(ctx, sessionID)
}

// IsInterviewActive delegates to the wrapped store.
func (c *CachingSessionStore) IsInterviewActive(ctx context.Context, sessionID string) (bool, error) {
	return c.store.IsInterviewActive(ctx, sessionID)
}

// GetSessionState delegates to the wrapped store. It stays a pure pass-through:
// the snapshot also carries the mutable deadpool/interview fields read every chat
// turn, so it cannot be served from the verified-only cache (phase-2 territory).
func (c *CachingSessionStore) GetSessionState(ctx context.Context, sessionID string) (server.SessionState, error) {
	return c.store.GetSessionState(ctx, sessionID)
}

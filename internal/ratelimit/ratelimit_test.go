package ratelimit

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// TestLimiter_PerKeyInterval verifies a single key is allowed at most once per
// perKey interval: the first call passes, an immediate second is throttled, and
// after the interval elapses it passes again.
func TestLimiter_PerKeyInterval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := New(time.Second, 1000)

		if !l.Allow("k") {
			t.Fatal("first call must be allowed")
		}
		if l.Allow("k") {
			t.Fatal("immediate second call must be throttled")
		}
		time.Sleep(time.Second)
		if !l.Allow("k") {
			t.Fatal("call after the interval must be allowed")
		}
	})
}

// TestLimiter_KeysAreIndependent verifies that one key's cooldown does not
// throttle a different key.
func TestLimiter_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := New(time.Second, 1000)

		if !l.Allow("a") {
			t.Fatal("first call for a must be allowed")
		}
		if !l.Allow("b") {
			t.Fatal("first call for b must be allowed (independent key)")
		}
		if l.Allow("a") {
			t.Fatal("second call for a must be throttled")
		}
	})
}

// TestLimiter_GlobalCeiling verifies the process-wide cap: with distinct keys
// (so the per-key interval never bites) only globalPerHour calls pass within the
// hour, and the budget refills after an hour elapses.
func TestLimiter_GlobalCeiling(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const global = 5
		l := New(time.Second, global)

		for i := 0; i < global; i++ {
			if !l.Allow(fmt.Sprintf("key-%d", i)) {
				t.Fatalf("call %d of %d under the global cap must be allowed", i+1, global)
			}
		}
		if l.Allow("key-over") {
			t.Fatal("call exceeding the global cap must be throttled")
		}

		// One refill token arrives every hour/global; sleep a full hour to be sure.
		time.Sleep(time.Hour)
		if !l.Allow("key-after") {
			t.Fatal("global budget must refill after an hour")
		}
	})
}

// TestLimiter_IdleEviction verifies the keys map does not grow without bound:
// keys untouched past perKey are swept, so a burst of one-shot keys does not
// leak entries.
func TestLimiter_IdleEviction(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := New(time.Second, 100000)

		for i := 0; i < 50; i++ {
			l.Allow(fmt.Sprintf("burst-%d", i))
		}

		// Advance well past perKey so every burst key is stale, then
		// make one more call to trigger the amortized sweep.
		time.Sleep(2 * time.Second)
		l.Allow("trigger")

		l.mu.Lock()
		n := len(l.keys)
		l.mu.Unlock()

		// Only the freshly-seen "trigger" key should remain.
		if n != 1 {
			t.Fatalf("keys map size = %d, want 1 after idle eviction", n)
		}
	})
}

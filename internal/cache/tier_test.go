package cache_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/cache"
)

// TestNew_Validation covers the constructor's input contract: a missing self IP
// or an unparseable / port-less listen addr must fail fast with a descriptive
// error rather than constructing a misconfigured universe.
func TestNew_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*cache.Config)
		wantErr string
	}{
		{
			name:    "empty self IP",
			mutate:  func(c *cache.Config) { c.SelfIP = "" },
			wantErr: "self IP",
		},
		{
			name:    "listen addr missing port",
			mutate:  func(c *cache.Config) { c.ListenAddr = "noport" },
			wantErr: "invalid listen addr",
		},
		{
			name:    "listen addr empty port",
			mutate:  func(c *cache.Config) { c.ListenAddr = "127.0.0.1:" },
			wantErr: "no port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tt.mutate(&cfg)
			tier, err := cache.New(cfg, newFakeStore(), testLogger())
			if err == nil {
				_ = tier.Close()
				t.Fatalf("New: got nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New error: got %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestNew_Succeeds asserts a valid config yields a tier whose Store() decorator
// and Handler() peer endpoint are both non-nil and ready to wire up.
func TestNew_Succeeds(t *testing.T) {
	t.Parallel()
	tier, err := cache.New(testConfig(), newFakeStore(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tier.Close() })

	if tier.Store() == nil {
		t.Error("Store() returned nil")
	}
	if tier.Handler() == nil {
		t.Error("Handler() returned nil")
	}
}

// TestTier_SingleReplicaGetterRunsLocally is the single-replica invariant: with
// self the sole peer (no RefreshPeers / listener), galaxy.Get resolves locally
// through the backend getter, so a verified session reads (true, nil) end to end.
func TestTier_SingleReplicaGetterRunsLocally(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.setVerified(true, nil)
	tier, err := cache.New(testConfig(), store, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tier.Close() })

	got, err := tier.Store().IsSessionVerified(context.Background(), "sess-local")
	if err != nil {
		t.Fatalf("IsSessionVerified: %v", err)
	}
	if !got {
		t.Error("got false; the local getter should have served the verified session")
	}
	if n := store.count("IsSessionVerified"); n != 1 {
		t.Errorf("backend getter ran %d times, want 1 (getter must run locally)", n)
	}
}

// TestTier_RegisterMetrics confirms the OTel observable-counter bridge registers
// cleanly against the no-op meter used in tests (CLI/test-safe path).
func TestTier_RegisterMetrics(t *testing.T) {
	t.Parallel()
	tier, err := cache.New(testConfig(), newFakeStore(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tier.Close() })

	if err := tier.RegisterMetrics(); err != nil {
		t.Errorf("RegisterMetrics: %v", err)
	}
}

// TestTier_Close is idempotent-enough for shutdown: a freshly built tier closes
// without error.
func TestTier_Close(t *testing.T) {
	t.Parallel()
	tier, err := cache.New(testConfig(), newFakeStore(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tier.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

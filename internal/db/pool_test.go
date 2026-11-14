package db_test

import "testing"

// TestNewPool_MaxConnsFitsSharedBudget pins the per-process pool cap. The
// db-f1-micro instance is shared with password-exchange (ADR 0015), so the
// server's 2 replicas may hold at most 8 connections between them.
func TestNewPool_MaxConnsFitsSharedBudget(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	const replicas, sreBibleBudget = 2, 8
	got := pool.Config().MaxConns
	if got != 4 {
		t.Fatalf("MaxConns = %d, want 4", got)
	}
	if replicas*int(got) > sreBibleBudget {
		t.Fatalf("%d replicas × %d conns exceeds sre-bible's shared budget of %d", replicas, got, sreBibleBudget)
	}
}

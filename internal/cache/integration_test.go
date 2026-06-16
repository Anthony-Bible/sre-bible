package cache_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/cache"
	"github.com/Anthony-Bible/sre-bible/internal/db"
)

// testCacheDB bootstraps a migrated pgxpool and returns a cleanup that truncates
// sessions CASCADE and closes the pool. It skips when TEST_DATABASE_URL is unset.
// The bootstrap order mirrors db.testSessionDB: a plain pool runs migrations, then
// a full db.NewPool (with the pgvector type hook) is used for the test body.
//
// NOTE: TEST_DATABASE_URL must point at the local disposable Postgres (port 5432),
// never the cloud-sql-proxy on 5433 — that proxies production.
func testCacheDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	bootstrapPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap pool: %v", err)
	}
	if err := db.Migrate(ctx, bootstrapPool, slog.Default()); err != nil {
		bootstrapPool.Close()
		t.Fatalf("Migrate: %v", err)
	}
	bootstrapPool.Close()

	pool, err := db.NewPool(ctx, dsn, slog.Default())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	t.Cleanup(func() {
		if _, cleanErr := pool.Exec(context.Background(), "TRUNCATE sessions CASCADE"); cleanErr != nil {
			t.Errorf("cleanup truncate: %v", cleanErr)
		}
		pool.Close()
	})

	if _, err := pool.Exec(ctx, "TRUNCATE sessions CASCADE"); err != nil {
		pool.Close()
		t.Fatalf("pre-test truncate: %v", err)
	}

	return pool
}

// TestIntegration_VerifiedFlipThroughCache exercises the full tier over a real DB:
// an unverified session reads false (getter NotFound, nothing cached), then after
// MarkSessionVerified the next lookup reads true through the backend getter and is
// served from the galaxy on the repeat — the monotonic flip with no TTL wait.
func TestIntegration_VerifiedFlipThroughCache(t *testing.T) {
	pool := testCacheDB(t)
	log := testLogger()
	store := db.NewSessionStore(pool, log)
	tier, err := cache.New(testConfig(), store, log)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { _ = tier.Close() })
	repo := tier.Store()

	ctx := context.Background()
	const id = "00000000-0000-4000-8000-cac4ecac4eca"

	if err := repo.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Unverified: false is never cached.
	verified, err := repo.IsSessionVerified(ctx, id)
	if err != nil {
		t.Fatalf("IsSessionVerified pre-mark: %v", err)
	}
	if verified {
		t.Fatal("pre-mark: got true, want false")
	}

	// Flip the monotonic flag in the DB.
	if err := repo.MarkSessionVerified(ctx, id); err != nil {
		t.Fatalf("MarkSessionVerified: %v", err)
	}

	// The flip is seen immediately on the next lookup (false was not cached).
	verified, err = repo.IsSessionVerified(ctx, id)
	if err != nil {
		t.Fatalf("IsSessionVerified post-mark: %v", err)
	}
	if !verified {
		t.Fatal("post-mark: got false, want true")
	}

	// Repeat lookup is served from the cache and still reads true.
	verified, err = repo.IsSessionVerified(ctx, id)
	if err != nil {
		t.Fatalf("IsSessionVerified cached: %v", err)
	}
	if !verified {
		t.Error("cached lookup: got false, want true")
	}
}

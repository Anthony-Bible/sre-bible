package db

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// NewPool creates a pgx connection pool with pgvector types registered.
// Pool limits are sized for db-f1-micro: 2 replicas × 5 conns + headroom for the ingest CLI.
func NewPool(ctx context.Context, databaseURL string, log *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	cfg.MaxConns = 5
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute

	// Bound every statement at the DB so a slow/stuck query can't pin one of the five
	// pooled connections indefinitely and starve the rest under a traffic spike. This is
	// the DB-side backstop; the request-side load-shed (a short context deadline that
	// bounds pool-acquire wait → HTTP 503) lives in internal/server. Set on ConnConfig
	// before the Copy() below so the bootstrap connection inherits it too (its lone
	// CREATE EXTENSION statement is trivially fast).
	//
	// Caveat: migrations run via goose on this same pool at startup (cmd/server/main.go →
	// internal/db/migrate.go), so this cap also applies to migration DDL. Current
	// migrations are tiny idempotent ADD COLUMN / guarded-CREATE ops and are safe under
	// 5s. Any future heavy DDL (e.g. building an ivfflat/hnsw index on a populated
	// embeddings table) MUST override it locally with `SET LOCAL statement_timeout = 0;`
	// at the top of that migration. See ADR 0014.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000" // milliseconds

	// Bootstrap the pgvector extension before wiring RegisterTypes below.
	// RegisterTypes looks up the `vector` type OID on every new connection and
	// errors ("vector type not found") if the extension was never created. But
	// CREATE EXTENSION lives in a migration that can't run until the pool can
	// connect — a chicken-and-egg on a fresh database. Create it up front on a
	// throwaway connection (which does not register vector types) so the typed
	// pool below can connect. IF NOT EXISTS makes this a no-op once bootstrapped,
	// and the privilege it needs is the same one the migration already uses.
	if err := ensureVectorExtension(ctx, cfg.ConnConfig.Copy()); err != nil {
		return nil, err
	}

	cfg.AfterConnect = pgxvector.RegisterTypes

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	log.InfoContext(ctx, "database pool ready", slog.String("url", redactDSN(databaseURL)))
	return pool, nil
}

// ensureVectorExtension creates the pgvector extension on a single throwaway
// connection that does not register vector types, so a fresh database can be
// bootstrapped before the typed pool (which requires the type to exist) opens.
func ensureVectorExtension(ctx context.Context, connCfg *pgx.ConnConfig) error {
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return fmt.Errorf("bootstrap connection: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("create vector extension: %w", err)
	}
	return nil
}

// redactDSN strips the password from a postgres DSN before it is logged.
// If the URL cannot be parsed the raw string is replaced with a placeholder.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "[unparseable dsn]"
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

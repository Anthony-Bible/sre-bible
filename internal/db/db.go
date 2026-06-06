package db

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

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

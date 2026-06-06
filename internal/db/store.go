package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// Source is a document or URL in the knowledge base.
// ID is assigned by the database on first insert and should be treated as read-only by callers.
type Source struct {
	ID       int64
	Name     string
	Type     string
	Location string
}

// Chunk is a contiguous text segment of a Source together with its vector embedding.
// Idx is the zero-based position of the chunk within its parent source and must be
// unique per source; callers are responsible for assigning sequential values.
type Chunk struct {
	Idx       int
	Content   string
	Embedding []float32
}

// SourceStore persists sources and chunks.
type SourceStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewSourceStore creates a SourceStore backed by pool.
// Pass a non-nil logger to route structured log output; if logger is nil, slog.Default() is used.
func NewSourceStore(pool *pgxpool.Pool, logger *slog.Logger) *SourceStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourceStore{pool: pool, logger: logger}
}

// ReplaceSource atomically upserts the source row and replaces all its chunks.
func (s *SourceStore) ReplaceSource(ctx context.Context, src Source, chunks []Chunk) error {
	start := time.Now()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op; the error is intentionally discarded

	sourceID, err := upsertSource(ctx, tx, src)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("delete old chunks: %w", err)
	}

	if err := insertChunks(ctx, tx, sourceID, chunks); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	s.logger.InfoContext(ctx, "source replaced",
		"name", src.Name,
		"chunks", len(chunks),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

func upsertSource(ctx context.Context, tx pgx.Tx, src Source) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO sources (name, type, location, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (name) DO UPDATE
		  SET type       = EXCLUDED.type,
		      location   = EXCLUDED.location,
		      updated_at = now()
		RETURNING id`,
		src.Name, src.Type, src.Location,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert source: %w", err)
	}
	return id, nil
}

func insertChunks(ctx context.Context, tx pgx.Tx, sourceID int64, chunks []Chunk) error {
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		rows[i] = []any{sourceID, c.Idx, c.Content, pgvector.NewVector(c.Embedding)}
	}

	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"chunks"},
		[]string{"source_id", "idx", "content", "embedding"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("insert chunks: %w", err)
	}
	return nil
}

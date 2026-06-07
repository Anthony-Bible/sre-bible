package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/email"
)

// compile-time assertion.
var _ email.ContactRepository = (*ContactStore)(nil)

// ContactStore persists contact email records for rate-limiting and audit.
type ContactStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewContactStore creates a ContactStore backed by pool.
func NewContactStore(pool *pgxpool.Pool, logger *slog.Logger) *ContactStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ContactStore{pool: pool, logger: logger}
}

// CountSince returns the number of contact emails created after t.
func (s *ContactStore) CountSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM contact_emails WHERE created_at > $1`, t,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count contact emails: %w", err)
	}
	return n, nil
}

// RecordSend inserts a contact email record and returns the new row ID.
// Returns email.ErrSessionAlreadySent on a unique index violation (code 23505).
//
// Note: CountSince + RecordSend is not atomic; the global cap may overshoot by
// at most the number of concurrent replicas — acceptable for abuse protection.
func (s *ContactStore) RecordSend(ctx context.Context, sessionID string, e email.ContactEmail) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO contact_emails (session_id, sender_name, sender_email)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		sessionID, e.SenderName, e.SenderEmail,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, email.ErrSessionAlreadySent
		}
		return 0, fmt.Errorf("record contact email send: %w", err)
	}
	return id, nil
}

// DeleteSend removes a contact email record by ID (compensating action on transport failure).
func (s *ContactStore) DeleteSend(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM contact_emails WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("delete contact email send: %w", err)
	}
	return nil
}

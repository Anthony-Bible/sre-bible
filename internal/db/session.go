package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/server"
)

// SessionStore persists anonymous chat sessions and their messages.
type SessionStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewSessionStore creates a SessionStore backed by pool.
func NewSessionStore(pool *pgxpool.Pool, logger *slog.Logger) *SessionStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionStore{pool: pool, logger: logger}
}

// CreateSession inserts a session row idempotently — safe to call on every request.
func (s *SessionStore) CreateSession(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id) VALUES ($1) ON CONFLICT DO NOTHING`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// ListMessages returns all messages for the session ordered by creation time.
func (s *SessionStore) ListMessages(ctx context.Context, sessionID string) ([]server.StoredMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT role, content, citations
		 FROM messages
		 WHERE session_id = $1
		 ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	msgs := make([]server.StoredMessage, 0)
	for rows.Next() {
		var role string
		var content string
		var citations []string
		if err := rows.Scan(&role, &content, &citations); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msgs = append(msgs, server.StoredMessage{
			Message:   rag.Message{Role: rag.Role(role), Content: content},
			Citations: citations,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages rows: %w", err)
	}
	return msgs, nil
}

// AppendMessage inserts a message into the session.
// Pass nil citations for user messages; they are stored as an empty array.
func (s *SessionStore) AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string) error {
	if citations == nil {
		citations = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO messages (session_id, role, content, citations) VALUES ($1, $2, $3, $4)`,
		sessionID, string(msg.Role), msg.Content, citations,
	)
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

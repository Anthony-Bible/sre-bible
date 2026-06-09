package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
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
// A NULL trace column (user turns, legacy rows) yields a nil Trace. A trace that
// fails to unmarshal is logged and degraded to nil so one corrupt row never fails
// the whole list.
func (s *SessionStore) ListMessages(ctx context.Context, sessionID string) ([]server.StoredMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT role, content, citations, trace
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
		var traceJSON []byte
		if err := rows.Scan(&role, &content, &citations, &traceJSON); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		var trace []rag.TraceStep
		if len(traceJSON) > 0 {
			if err := json.Unmarshal(traceJSON, &trace); err != nil {
				s.logger.WarnContext(ctx, "unmarshal message trace; degrading to nil",
					slog.Any("err", err), slog.String("session", sessionID))
				trace = nil
			}
		}
		msgs = append(msgs, server.StoredMessage{
			Message:   rag.Message{Role: rag.Role(role), Content: content},
			Citations: citations,
			Trace:     trace,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages rows: %w", err)
	}
	return msgs, nil
}

// AppendMessage inserts a message into the session.
// Pass nil citations for user messages; they are stored as an empty array.
// Pass nil (or empty) trace for user messages and untraced turns; an empty trace is
// stored as SQL NULL (distinguishing "no trace" from a recorded-but-empty trace).
func (s *SessionStore) AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string, trace []rag.TraceStep) error {
	if citations == nil {
		citations = []string{}
	}
	var traceJSON []byte
	if len(trace) > 0 {
		b, err := json.Marshal(trace)
		if err != nil {
			return fmt.Errorf("marshal trace: %w", err)
		}
		traceJSON = b
	}
	// traceJSON == nil → pgx writes SQL NULL for the JSONB column.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO messages (session_id, role, content, citations, trace) VALUES ($1, $2, $3, $4, $5)`,
		sessionID, string(msg.Role), msg.Content, citations, traceJSON,
	)
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

// IsSessionVerified returns true when the session's Turnstile verification has been confirmed.
// Returns (false, nil) when the session does not exist.
func (s *SessionStore) IsSessionVerified(ctx context.Context, sessionID string) (bool, error) {
	var verified bool
	err := s.pool.QueryRow(ctx,
		`SELECT turnstile_verified FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is session verified: %w", err)
	}
	return verified, nil
}

// MarkSessionVerified sets turnstile_verified = true for the given session.
func (s *SessionStore) MarkSessionVerified(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET turnstile_verified = true WHERE id = $1`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("mark session verified: %w", err)
	}
	return nil
}

// GetInterviewState returns the persisted interview state for the session.
// Returns (nil, nil) when the session does not exist or interview_state is SQL NULL.
func (s *SessionStore) GetInterviewState(ctx context.Context, sessionID string) (*rag.InterviewState, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT interview_state FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get interview state: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var state rag.InterviewState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("unmarshal interview state: %w", err)
	}
	return &state, nil
}

// SetInterviewState persists state and marks the session's interview as active.
func (s *SessionStore) SetInterviewState(ctx context.Context, sessionID string, state *rag.InterviewState) error {
	if state == nil {
		return errors.New("interview state is nil")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal interview state: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE sessions SET interview_state = $1, interview_active = true WHERE id = $2`,
		raw, sessionID,
	)
	if err != nil {
		return fmt.Errorf("set interview state: %w", err)
	}
	return nil
}

// ClearInterviewState clears the persisted interview state and marks it inactive.
func (s *SessionStore) ClearInterviewState(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET interview_state = NULL, interview_active = false WHERE id = $1`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("clear interview state: %w", err)
	}
	return nil
}

// IsInterviewActive returns true when the session has an active interview.
// Returns (false, nil) if the session does not exist.
func (s *SessionStore) IsInterviewActive(ctx context.Context, sessionID string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT interview_active FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is interview active: %w", err)
	}
	return active, nil
}

// SetDeadpoolMode sets the deadpool_mode column for the given session.
func (s *SessionStore) SetDeadpoolMode(ctx context.Context, sessionID string, enabled bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET deadpool_mode = $1 WHERE id = $2`,
		enabled, sessionID,
	)
	if err != nil {
		return fmt.Errorf("set deadpool mode: %w", err)
	}
	return nil
}

// IsDeadpoolMode returns true if the session is currently in Deadpool Mode.
// Returns (false, nil) if the session does not exist.
func (s *SessionStore) IsDeadpoolMode(ctx context.Context, sessionID string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT deadpool_mode FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is deadpool mode: %w", err)
	}
	return enabled, nil
}

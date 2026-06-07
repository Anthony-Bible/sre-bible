package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/email"
)

// seedSession inserts a sessions row and returns its UUID as a string.
func seedSession(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO sessions DEFAULT VALUES RETURNING id::text`,
	).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

// TestContactStore_RecordSend_DuplicateReturnsErrSessionAlreadySent verifies
// that a second RecordSend for the same session maps to ErrSessionAlreadySent.
func TestContactStore_RecordSend_DuplicateReturnsErrSessionAlreadySent(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	sid := seedSession(t, pool)
	store := db.NewContactStore(pool, nil)
	e := email.ContactEmail{SenderName: "Alice", SenderEmail: "alice@example.com"}

	if _, err := store.RecordSend(context.Background(), sid, e); err != nil {
		t.Fatalf("first RecordSend: %v", err)
	}
	_, err := store.RecordSend(context.Background(), sid, e)
	if !errors.Is(err, email.ErrSessionAlreadySent) {
		t.Errorf("second RecordSend: got %v, want email.ErrSessionAlreadySent", err)
	}
}

// TestContactStore_RecordSend_DifferentSessionsAllowed verifies that two
// distinct sessions can each record one send without error.
func TestContactStore_RecordSend_DifferentSessionsAllowed(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	sidA := seedSession(t, pool)
	sidB := seedSession(t, pool)
	store := db.NewContactStore(pool, nil)
	e := email.ContactEmail{SenderName: "Alice", SenderEmail: "alice@example.com"}

	if _, err := store.RecordSend(context.Background(), sidA, e); err != nil {
		t.Fatalf("RecordSend A: %v", err)
	}
	if _, err := store.RecordSend(context.Background(), sidB, e); err != nil {
		t.Fatalf("RecordSend B: %v", err)
	}
}

// TestContactStore_CountSince_Window verifies that CountSince counts only rows
// with created_at strictly after the provided threshold.
func TestContactStore_CountSince_Window(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	sidA := seedSession(t, pool)
	sidB := seedSession(t, pool)
	sidC := seedSession(t, pool)

	store := db.NewContactStore(pool, nil)
	e := email.ContactEmail{SenderName: "X", SenderEmail: "x@example.com"}

	if _, err := store.RecordSend(context.Background(), sidA, e); err != nil {
		t.Fatalf("RecordSend A: %v", err)
	}
	if _, err := store.RecordSend(context.Background(), sidB, e); err != nil {
		t.Fatalf("RecordSend B: %v", err)
	}

	// Backdate B so it falls outside the 1-hour window.
	if _, err := pool.Exec(context.Background(),
		`UPDATE contact_emails SET created_at = now() - interval '2 hours'
		 WHERE session_id = $1`, sidB,
	); err != nil {
		t.Fatalf("backdate B: %v", err)
	}

	if _, err := store.RecordSend(context.Background(), sidC, e); err != nil {
		t.Fatalf("RecordSend C: %v", err)
	}

	// Count within the last hour — should include A and C but not backdated B.
	n, err := store.CountSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountSince: %v", err)
	}
	if n != 2 {
		t.Errorf("CountSince: got %d, want 2 (A + C)", n)
	}
}

// TestContactStore_DeleteSend_RemovesRecord verifies that DeleteSend removes
// the row and CountSince reflects the deletion.
func TestContactStore_DeleteSend_RemovesRecord(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	sid := seedSession(t, pool)
	store := db.NewContactStore(pool, nil)
	e := email.ContactEmail{SenderName: "Alice", SenderEmail: "alice@example.com"}

	id, err := store.RecordSend(context.Background(), sid, e)
	if err != nil {
		t.Fatalf("RecordSend: %v", err)
	}

	if err := store.DeleteSend(context.Background(), id); err != nil {
		t.Fatalf("DeleteSend: %v", err)
	}

	n, err := store.CountSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountSince after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("CountSince after delete: got %d, want 0", n)
	}
}

package db_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// testSessionDB sets up a pgxpool with full migrations applied, skipping when
// TEST_DATABASE_URL is not set.  The returned cleanup func truncates sessions
// CASCADE (which cascades to messages) and closes the pool.
//
// Bootstrap order mirrors testDB:
//  1. Plain pool — no pgvector AfterConnect hook — solely for running migrations.
//  2. Swap to the full db.NewPool once the extension is confirmed present.
func testSessionDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	// Step 1: plain pool for migrations only.
	bootstrapPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap pool: %v", err)
	}
	if err := db.Migrate(ctx, bootstrapPool, slog.Default()); err != nil {
		bootstrapPool.Close()
		t.Fatalf("Migrate: %v", err)
	}
	bootstrapPool.Close()

	// Step 2: full pool with pgvector types registered.
	pool, err := db.NewPool(ctx, dsn, slog.Default())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	cleanup := func() {
		_, cleanErr := pool.Exec(context.Background(), "TRUNCATE sessions CASCADE")
		if cleanErr != nil {
			t.Errorf("cleanup truncate: %v", cleanErr)
		}
		pool.Close()
	}

	// Ensure a clean slate before the test body runs.
	if _, err := pool.Exec(ctx, "TRUNCATE sessions CASCADE"); err != nil {
		pool.Close()
		t.Fatalf("pre-test truncate: %v", err)
	}

	return pool, cleanup
}

// sessionID returns a deterministic UUID-format string for use as a session
// identifier in tests.  Each call with a distinct suffix yields a distinct ID
// so tests can run in parallel without colliding.
func sessionID(suffix string) string {
	// UUID v4 format: 8-4-4-4-12 hex chars; we build a valid-looking string
	// from a fixed prefix and a test-supplied suffix (padded to fit).
	padded := (suffix + "000000000000")[:12]
	return "00000000-0000-4000-8000-" + padded
}

// countSessionRows is a thin wrapper around countRows for sessions queries.
func countSessionRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	return countRows(t, pool, query, args...)
}

// --- Contract 1: CreateSession idempotency ---

// TestCreateSession_Idempotent verifies that calling CreateSession twice with
// the same session ID produces exactly one row in the sessions table and
// returns no error on either call.
func TestCreateSession_Idempotent(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("aaaaaaaaaaaa")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}
	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("second CreateSession (must not error on duplicate): %v", err)
	}

	n := countSessionRows(t, pool, `SELECT COUNT(*) FROM sessions WHERE id = $1`, id)
	if n != 1 {
		t.Errorf("expected exactly 1 session row after two CreateSession calls, got %d", n)
	}
}

// --- Contract 2: ListMessages on an empty session ---

// TestListMessages_EmptySession verifies that ListMessages returns an empty,
// non-nil slice (and no error) immediately after CreateSession with no
// messages appended.
func TestListMessages_EmptySession(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("bbbbbbbbbbbb")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages on empty session: %v", err)
	}
	// Contract: caller must be able to range over the result without a nil check.
	if msgs == nil {
		t.Error("ListMessages returned nil; want empty non-nil slice")
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for fresh session, got %d", len(msgs))
	}
}

// --- Contract 3: AppendMessage — user message with nil citations ---

// TestAppendMessage_UserNoCitations verifies that a user message appended with
// a nil citations slice is stored and retrieved with an empty (len == 0)
// citations slice — never nil, never populated with phantom entries.
func TestAppendMessage_UserNoCitations(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("cccccccccccc")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	msg := rag.Message{Role: rag.RoleUser, Content: "hello from the crawler"}
	if err := store.AppendMessage(ctx, id, msg, nil, nil); err != nil {
		t.Fatalf("AppendMessage with nil citations: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	got := msgs[0]
	if got.Role != rag.RoleUser {
		t.Errorf("role: got %q, want %q", got.Role, rag.RoleUser)
	}
	if got.Content != msg.Content {
		t.Errorf("content: got %q, want %q", got.Content, msg.Content)
	}
	// Contract: nil citations in → empty citations slice out (len == 0).
	if len(got.Citations) != 0 {
		t.Errorf("citations: got %v (len %d), want empty slice", got.Citations, len(got.Citations))
	}
}

// --- Contract 4: AppendMessage — assistant message with citations ---

// TestAppendMessage_AssistantWithCitations verifies that an assistant message
// appended with a non-empty citations slice is retrieved with exactly those
// citations, in the same order.
func TestAppendMessage_AssistantWithCitations(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("dddddddddddd")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	wantCitations := []string{"resume.pdf", "anthonybible.com"}
	msg := rag.Message{Role: rag.RoleAssistant, Content: "here is your answer, crawler"}
	if err := store.AppendMessage(ctx, id, msg, wantCitations, nil); err != nil {
		t.Fatalf("AppendMessage with citations: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	got := msgs[0]
	if got.Role != rag.RoleAssistant {
		t.Errorf("role: got %q, want %q", got.Role, rag.RoleAssistant)
	}
	if got.Content != msg.Content {
		t.Errorf("content: got %q, want %q", got.Content, msg.Content)
	}
	if len(got.Citations) != len(wantCitations) {
		t.Fatalf("citations length: got %d, want %d", len(got.Citations), len(wantCitations))
	}
	for i, want := range wantCitations {
		if got.Citations[i] != want {
			t.Errorf("citations[%d]: got %q, want %q", i, got.Citations[i], want)
		}
	}
}

// --- Contract 4b: AppendMessage — assistant message with Agent Trace ---

// TestAppendMessage_AssistantWithTrace_RoundTrip verifies that a non-empty Agent Trace
// persisted with an assistant turn survives a round-trip through the JSONB column with its
// structured detail intact (kinds, counts, grounding excerpts, tool-call target/outcome).
func TestAppendMessage_AssistantWithTrace_RoundTrip(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("a1a1a1a1a1a1")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	wantTrace := []rag.TraceStep{
		{
			Kind:  rag.TraceKindRetrieval,
			Label: "Searched knowledge base",
			Retrieval: &rag.RetrievalDetail{
				ChunkCount:  2,
				SourceCount: 1,
				Excerpts:    []rag.GroundingExcerpt{{SourceName: "resume.pdf", Text: "grounding excerpt"}},
			},
		},
		{
			Kind:     rag.TraceKindToolCall,
			Label:    "Reading resume.pdf…",
			ToolCall: &rag.ToolCallDetail{Tool: "fetch_full_document", Target: "resume.pdf", Outcome: "ok"},
		},
		{
			Kind:   rag.TraceKindAnswer,
			Label:  "Composed answer",
			Answer: &rag.AnswerDetail{ToolRounds: 1, DurationMs: 1234},
		},
	}
	msg := rag.Message{Role: rag.RoleAssistant, Content: "answer with trace"}
	if err := store.AppendMessage(ctx, id, msg, []string{"resume.pdf"}, wantTrace); err != nil {
		t.Fatalf("AppendMessage with trace: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	gotTrace := msgs[0].Trace
	if len(gotTrace) != len(wantTrace) {
		t.Fatalf("trace length: got %d, want %d", len(gotTrace), len(wantTrace))
	}

	// retrieval step
	if gotTrace[0].Kind != rag.TraceKindRetrieval || gotTrace[0].Retrieval == nil {
		t.Fatalf("trace[0]: got %+v, want a retrieval step with detail", gotTrace[0])
	}
	if gotTrace[0].Retrieval.ChunkCount != 2 || gotTrace[0].Retrieval.SourceCount != 1 {
		t.Errorf("retrieval counts: got chunk=%d source=%d, want 2/1",
			gotTrace[0].Retrieval.ChunkCount, gotTrace[0].Retrieval.SourceCount)
	}
	if len(gotTrace[0].Retrieval.Excerpts) != 1 || gotTrace[0].Retrieval.Excerpts[0].Text != "grounding excerpt" {
		t.Errorf("grounding excerpts not preserved: %+v", gotTrace[0].Retrieval.Excerpts)
	}

	// tool_call step
	if gotTrace[1].Kind != rag.TraceKindToolCall || gotTrace[1].ToolCall == nil {
		t.Fatalf("trace[1]: got %+v, want a tool_call step with detail", gotTrace[1])
	}
	if gotTrace[1].ToolCall.Target != "resume.pdf" || gotTrace[1].ToolCall.Outcome != "ok" {
		t.Errorf("tool_call detail: got %+v, want target=resume.pdf outcome=ok", gotTrace[1].ToolCall)
	}

	// answer step
	if gotTrace[2].Kind != rag.TraceKindAnswer || gotTrace[2].Answer == nil {
		t.Fatalf("trace[2]: got %+v, want an answer step with detail", gotTrace[2])
	}
	if gotTrace[2].Answer.ToolRounds != 1 || gotTrace[2].Answer.DurationMs != 1234 {
		t.Errorf("answer detail: got %+v, want rounds=1 durationMs=1234", gotTrace[2].Answer)
	}
}

// --- Contract 4c: ListMessages — NULL trace degrades to nil ---

// TestListMessages_NilTraceDegradesGracefully verifies that a message stored with no
// trace (a user turn, or a legacy assistant row) yields a nil Trace and no error — the
// NULL JSONB column must not break the list.
func TestListMessages_NilTraceDegradesGracefully(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("b2b2b2b2b2b2")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Append with an explicitly nil trace — the column is written as SQL NULL.
	msg := rag.Message{Role: rag.RoleUser, Content: "no trace here"}
	if err := store.AppendMessage(ctx, id, msg, nil, nil); err != nil {
		t.Fatalf("AppendMessage with nil trace: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Trace != nil {
		t.Errorf("Trace: got %v, want nil for a NULL trace column", msgs[0].Trace)
	}
}

// --- Contract 5: ListMessages ordering ---

// TestListMessages_OrderedByCreatedAt verifies that ListMessages returns
// messages in ascending creation-time order.  A user message is appended
// first, then an assistant message; the user message must come first in the
// result.
func TestListMessages_OrderedByCreatedAt(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("eeeeeeeeeeee")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	userMsg := rag.Message{Role: rag.RoleUser, Content: "user turn"}
	assistantMsg := rag.Message{Role: rag.RoleAssistant, Content: "assistant turn"}

	if err := store.AppendMessage(ctx, id, userMsg, nil, nil); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if err := store.AppendMessage(ctx, id, assistantMsg, []string{"src.pdf"}, nil); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}

	msgs, err := store.ListMessages(ctx, id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != rag.RoleUser {
		t.Errorf("msgs[0].Role: got %q, want %q (user must come first)", msgs[0].Role, rag.RoleUser)
	}
	if msgs[1].Role != rag.RoleAssistant {
		t.Errorf("msgs[1].Role: got %q, want %q", msgs[1].Role, rag.RoleAssistant)
	}
}

// --- Contract 6: Session isolation ---

// TestListMessages_SessionIsolation verifies that messages appended to one
// session are not visible when listing messages for a different session.
func TestListMessages_SessionIsolation(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	idA := sessionID("ffffffffffff")
	idB := sessionID("111111111111")
	ctx := context.Background()

	for _, id := range []string{idA, idB} {
		if err := store.CreateSession(ctx, id); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}

	msgA := rag.Message{Role: rag.RoleUser, Content: "session A message"}
	msgB := rag.Message{Role: rag.RoleUser, Content: "session B message"}

	if err := store.AppendMessage(ctx, idA, msgA, nil, nil); err != nil {
		t.Fatalf("AppendMessage session A: %v", err)
	}
	if err := store.AppendMessage(ctx, idB, msgB, nil, nil); err != nil {
		t.Fatalf("AppendMessage session B: %v", err)
	}

	msgsA, err := store.ListMessages(ctx, idA)
	if err != nil {
		t.Fatalf("ListMessages session A: %v", err)
	}
	if len(msgsA) != 1 {
		t.Fatalf("session A: expected 1 message, got %d", len(msgsA))
	}
	if msgsA[0].Content != msgA.Content {
		t.Errorf("session A message content: got %q, want %q", msgsA[0].Content, msgA.Content)
	}

	msgsB, err := store.ListMessages(ctx, idB)
	if err != nil {
		t.Fatalf("ListMessages session B: %v", err)
	}
	if len(msgsB) != 1 {
		t.Fatalf("session B: expected 1 message, got %d", len(msgsB))
	}
	if msgsB[0].Content != msgB.Content {
		t.Errorf("session B message content: got %q, want %q", msgsB[0].Content, msgB.Content)
	}
}

// --- Contract 7: IsSessionVerified default false ---

// TestIsSessionVerified_DefaultFalse verifies that a freshly created session
// has turnstile_verified = false before MarkSessionVerified is called.
func TestIsSessionVerified_DefaultFalse(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("777777777777")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	verified, err := store.IsSessionVerified(ctx, id)
	if err != nil {
		t.Fatalf("IsSessionVerified: %v", err)
	}
	if verified {
		t.Error("IsSessionVerified returned true for a fresh session, want false")
	}
}

// --- Contract 8: IsSessionVerified returns false for unknown session ---

// TestIsSessionVerified_UnknownSession verifies that IsSessionVerified returns
// (false, nil) when the session does not exist — no error, just false.
func TestIsSessionVerified_UnknownSession(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	ctx := context.Background()

	verified, err := store.IsSessionVerified(ctx, sessionID("888888888888"))
	if err != nil {
		t.Fatalf("IsSessionVerified for unknown session: %v", err)
	}
	if verified {
		t.Error("IsSessionVerified returned true for unknown session, want false")
	}
}

// --- Contract 9: MarkSessionVerified round-trip ---

// TestMarkSessionVerified_RoundTrip verifies that calling MarkSessionVerified
// causes IsSessionVerified to return true for the same session.
func TestMarkSessionVerified_RoundTrip(t *testing.T) {
	pool, cleanup := testSessionDB(t)
	defer cleanup()

	store := db.NewSessionStore(pool, slog.Default())
	id := sessionID("999999999999")
	ctx := context.Background()

	if err := store.CreateSession(ctx, id); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.MarkSessionVerified(ctx, id); err != nil {
		t.Fatalf("MarkSessionVerified: %v", err)
	}

	verified, err := store.IsSessionVerified(ctx, id)
	if err != nil {
		t.Fatalf("IsSessionVerified after mark: %v", err)
	}
	if !verified {
		t.Error("IsSessionVerified returned false after MarkSessionVerified, want true")
	}
}

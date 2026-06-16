package cache_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Anthony-Bible/sre-bible/internal/cache"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/server"
)

// Compile-time assertion that the decorator satisfies the consumption-site port.
// Mirrors the assertion block in cmd/server/main.go so a signature drift on
// server.SessionRepository fails this package's build too.
var _ server.SessionRepository = (*cache.CachingSessionStore)(nil)

// testLogger returns a logger that discards output to keep test logs clean while
// still exercising the decorator's log calls (e.g. the error path).
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// testConfig is a single-replica tier config: self is the sole peer, so every
// galaxy.Get runs the local getter (no HTTP listener is started in unit tests).
func testConfig() cache.Config {
	return cache.Config{
		SelfIP:      "127.0.0.1",
		ListenAddr:  ":9091",
		MaxBytes:    1 << 20,
		TTL:         time.Hour,
		PeerRefresh: time.Minute,
		HeadlessDNS: "localhost",
	}
}

// fakeStore is a server.SessionRepository test double that records per-method
// call counts and serves canned return values. IsSessionVerified's result is
// mutable mid-test (setVerified) so the false→true flip can be exercised.
type fakeStore struct {
	mu    sync.Mutex
	calls map[string]int

	verified  bool
	verifyErr error

	// Canned pass-through return values for the delegated methods.
	messages        []server.StoredMessage
	deadpool        bool
	interviewActive bool
	interviewState  *rag.InterviewState
	sessionState    server.SessionState
}

func newFakeStore() *fakeStore {
	return &fakeStore{calls: make(map[string]int)}
}

func (f *fakeStore) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeStore) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeStore) setVerified(v bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = v
	f.verifyErr = err
}

func (f *fakeStore) IsSessionVerified(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["IsSessionVerified"]++
	return f.verified, f.verifyErr
}

func (f *fakeStore) CreateSession(_ context.Context, _ string) error {
	f.record("CreateSession")
	return nil
}

func (f *fakeStore) ListMessages(_ context.Context, _ string) ([]server.StoredMessage, error) {
	f.record("ListMessages")
	return f.messages, nil
}

func (f *fakeStore) AppendMessage(_ context.Context, _ string, _ rag.Message, _ []string, _ []rag.TraceStep) error {
	f.record("AppendMessage")
	return nil
}

func (f *fakeStore) MarkSessionVerified(_ context.Context, _ string) error {
	f.record("MarkSessionVerified")
	return nil
}

func (f *fakeStore) SetDeadpoolMode(_ context.Context, _ string, _ bool) error {
	f.record("SetDeadpoolMode")
	return nil
}

func (f *fakeStore) IsDeadpoolMode(_ context.Context, _ string) (bool, error) {
	f.record("IsDeadpoolMode")
	return f.deadpool, nil
}

func (f *fakeStore) GetInterviewState(_ context.Context, _ string) (*rag.InterviewState, error) {
	f.record("GetInterviewState")
	return f.interviewState, nil
}

func (f *fakeStore) SetInterviewState(_ context.Context, _ string, _ *rag.InterviewState) error {
	f.record("SetInterviewState")
	return nil
}

func (f *fakeStore) ClearInterviewState(_ context.Context, _ string) error {
	f.record("ClearInterviewState")
	return nil
}

func (f *fakeStore) IsInterviewActive(_ context.Context, _ string) (bool, error) {
	f.record("IsInterviewActive")
	return f.interviewActive, nil
}

func (f *fakeStore) GetSessionState(_ context.Context, _ string) (server.SessionState, error) {
	f.record("GetSessionState")
	return f.sessionState, nil
}

// newDecorator builds a single-replica caching decorator over store and registers
// tier shutdown for cleanup.
func newDecorator(t *testing.T, store server.SessionRepository) server.SessionRepository {
	t.Helper()
	tier, err := cache.New(testConfig(), store, testLogger())
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { _ = tier.Close() })
	return tier.Store()
}

// TestIsSessionVerified_CachesTrue is the core saving: a verified session is read
// from the DB once, then served from the galaxy on every subsequent lookup.
func TestIsSessionVerified_CachesTrue(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.setVerified(true, nil)
	repo := newDecorator(t, store)
	ctx := context.Background()

	for i := range 3 {
		got, err := repo.IsSessionVerified(ctx, "sess-verified")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !got {
			t.Fatalf("call %d: got false, want true", i)
		}
	}

	// The backend getter — and therefore the store — is consulted exactly once;
	// the 2nd and 3rd lookups are served from the cache.
	if n := store.count("IsSessionVerified"); n != 1 {
		t.Errorf("store IsSessionVerified calls: got %d, want 1 (cache should serve repeats)", n)
	}
}

// TestIsSessionVerified_FalseNotCached proves "false" is never cached: every
// lookup of an unverified session re-reads the DB so a flip is seen immediately.
func TestIsSessionVerified_FalseNotCached(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.setVerified(false, nil)
	repo := newDecorator(t, store)
	ctx := context.Background()

	for i := range 2 {
		got, err := repo.IsSessionVerified(ctx, "sess-unverified")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got {
			t.Fatalf("call %d: got true, want false", i)
		}
	}

	if n := store.count("IsSessionVerified"); n != 2 {
		t.Errorf("store IsSessionVerified calls: got %d, want 2 (false must not be cached)", n)
	}
}

// TestIsSessionVerified_ErrorPropagates is the fail-closed contract: a backend
// error surfaces unchanged (so the gate refuses) and is never cached as a verdict.
func TestIsSessionVerified_ErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db is on fire")
	store := newFakeStore()
	store.setVerified(false, sentinel)
	repo := newDecorator(t, store)
	ctx := context.Background()

	got, err := repo.IsSessionVerified(ctx, "sess-error")
	if err == nil {
		t.Fatal("expected an error, got nil (gate would wrongly fail open)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain: got %v, want it to wrap %v", err, sentinel)
	}
	if got {
		t.Error("got true on error; the gate must not be told the session is verified")
	}

	// A second call still hits the store: the error verdict was not cached.
	if _, err := repo.IsSessionVerified(ctx, "sess-error"); err == nil {
		t.Fatal("expected an error on the 2nd call too")
	}
	if n := store.count("IsSessionVerified"); n != 2 {
		t.Errorf("store IsSessionVerified calls: got %d, want 2 (errors must not be cached)", n)
	}
}

// TestIsSessionVerified_FalseToTrueFlip proves the monotonic flip is observed on
// the very next lookup with no TTL wait, because "false" was never cached.
func TestIsSessionVerified_FalseToTrueFlip(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.setVerified(false, nil)
	repo := newDecorator(t, store)
	ctx := context.Background()

	got, err := repo.IsSessionVerified(ctx, "sess-flip")
	if err != nil {
		t.Fatalf("pre-flip: %v", err)
	}
	if got {
		t.Fatal("pre-flip: got true, want false")
	}

	store.setVerified(true, nil) // MarkSessionVerified equivalent

	got, err = repo.IsSessionVerified(ctx, "sess-flip")
	if err != nil {
		t.Fatalf("post-flip: %v", err)
	}
	if !got {
		t.Error("post-flip: got false, want true (flip must be seen immediately)")
	}
}

// TestIsSessionVerified_Concurrent is the -race guard: many concurrent lookups of
// a verified session must all return (true, nil) with no data race.
func TestIsSessionVerified_Concurrent(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.setVerified(true, nil)
	repo := newDecorator(t, store)
	ctx := context.Background()

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			got, err := repo.IsSessionVerified(ctx, "sess-concurrent")
			if err != nil {
				errs <- err
				return
			}
			if !got {
				errs <- errors.New("got false, want true")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent lookup: %v", err)
	}
}

// TestPassThroughMethods_Delegate verifies the eleven non-cached methods each
// delegate to the wrapped store and relay its return values unchanged.
func TestPassThroughMethods_Delegate(t *testing.T) {
	// Not parallel: the subtests assert per-method call counts on the shared
	// fakeStore, so they must run sequentially (and so must this parent, to keep
	// the tparallel linter consistent).
	store := newFakeStore()
	store.messages = []server.StoredMessage{{Citations: []string{"resume.pdf"}}}
	store.deadpool = true
	store.interviewActive = true
	store.interviewState = &rag.InterviewState{TotalQuestions: 3}
	store.sessionState = server.SessionState{Verified: true, DeadpoolMode: true}
	repo := newDecorator(t, store)
	ctx := context.Background()
	const id = "sess-delegate"

	// Each closure exercises one pass-through method.
	calls := []struct {
		method string
		invoke func() error
	}{
		{"CreateSession", func() error { return repo.CreateSession(ctx, id) }},
		{"ListMessages", func() error { _, err := repo.ListMessages(ctx, id); return err }},
		{"AppendMessage", func() error {
			return repo.AppendMessage(ctx, id, rag.Message{Role: rag.RoleUser, Content: "hi"}, nil, nil)
		}},
		{"MarkSessionVerified", func() error { return repo.MarkSessionVerified(ctx, id) }},
		{"SetDeadpoolMode", func() error { return repo.SetDeadpoolMode(ctx, id, true) }},
		{"IsDeadpoolMode", func() error { _, err := repo.IsDeadpoolMode(ctx, id); return err }},
		{"GetInterviewState", func() error { _, err := repo.GetInterviewState(ctx, id); return err }},
		{"SetInterviewState", func() error { return repo.SetInterviewState(ctx, id, store.interviewState) }},
		{"ClearInterviewState", func() error { return repo.ClearInterviewState(ctx, id) }},
		{"IsInterviewActive", func() error { _, err := repo.IsInterviewActive(ctx, id); return err }},
		{"GetSessionState", func() error { _, err := repo.GetSessionState(ctx, id); return err }},
	}

	for _, c := range calls {
		t.Run(c.method, func(t *testing.T) {
			if err := c.invoke(); err != nil {
				t.Fatalf("%s returned error: %v", c.method, err)
			}
			if n := store.count(c.method); n != 1 {
				t.Errorf("%s delegated %d times, want 1", c.method, n)
			}
		})
	}

	// Spot-check that return values are relayed, not swallowed.
	if msgs, _ := repo.ListMessages(ctx, id); len(msgs) != 1 {
		t.Errorf("ListMessages relayed %d messages, want 1", len(msgs))
	}
	if dp, _ := repo.IsDeadpoolMode(ctx, id); !dp {
		t.Error("IsDeadpoolMode relayed false, want true")
	}
	if active, _ := repo.IsInterviewActive(ctx, id); !active {
		t.Error("IsInterviewActive relayed false, want true")
	}
	if st, _ := repo.GetInterviewState(ctx, id); st == nil || st.TotalQuestions != 3 {
		t.Errorf("GetInterviewState relayed %+v, want TotalQuestions=3", st)
	}
	if ss, _ := repo.GetSessionState(ctx, id); !ss.Verified || !ss.DeadpoolMode {
		t.Errorf("GetSessionState relayed %+v, want Verified+DeadpoolMode", ss)
	}
}

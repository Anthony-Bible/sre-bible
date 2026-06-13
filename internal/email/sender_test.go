package email_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Anthony-Bible/sre-bible/internal/email"
)

// --- fakes ---

type fakeTransport struct {
	calls int
	err   error
}

func (f *fakeTransport) Send(_ context.Context, _ email.Message) error {
	f.calls++
	return f.err
}

type fakeRepo struct {
	records       []email.ContactEmail
	deleteIDs     []int64
	countSinceN   int
	countSinceErr error
	recordErr     error
	nextID        int64
}

func (r *fakeRepo) CountSince(_ context.Context, _ time.Time) (int, error) {
	return r.countSinceN, r.countSinceErr
}

func (r *fakeRepo) RecordSend(_ context.Context, _ string, e email.ContactEmail) (int64, error) {
	if r.recordErr != nil {
		return 0, r.recordErr
	}
	r.records = append(r.records, e)
	r.nextID++
	return r.nextID, nil
}

func (r *fakeRepo) DeleteSend(_ context.Context, id int64) error {
	r.deleteIDs = append(r.deleteIDs, id)
	return nil
}

func newSvc(repo email.ContactRepository, tx email.Transport, limit int) *email.Service {
	cfg := email.Config{
		From:        "from@example.com",
		To:          "to@example.com",
		GlobalLimit: limit,
		Window:      time.Hour,
	}
	return email.NewService(repo, tx, cfg, nil)
}

// --- tests ---

func TestSendContactEmail_HappyPath(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-1").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Alice",
		SenderEmail: "alice@acme.co",
		Message:     "Hello Anthony",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true, got false; reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason on success, got %q", reason)
	}
	if len(repo.records) != 1 {
		t.Errorf("expected 1 recorded send, got %d", len(repo.records))
	}
	if tx.calls != 1 {
		t.Errorf("expected 1 transport call, got %d", tx.calls)
	}
}

func TestSendContactEmail_AlreadySentRefusal(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{recordErr: email.ErrSessionAlreadySent}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-dup").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Bob",
		SenderEmail: "bob@acme.co",
		Message:     "Second message",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for already-sent session")
	}
	if reason == "" {
		t.Error("expected non-empty reason for already-sent")
	}
	if tx.calls != 0 {
		t.Errorf("transport must not be called, got %d calls", tx.calls)
	}
}

func TestSendContactEmail_GlobalCapHit(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{countSinceN: 5}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 5) // cap = 5, count = 5 → at cap

	ok, reason, err := svc.Bind("sess-cap").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Carol",
		SenderEmail: "carol@acme.co",
		Message:     "Message",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false when global cap is hit")
	}
	if reason == "" {
		t.Error("expected non-empty reason when cap is hit")
	}
	if len(repo.records) != 0 {
		t.Errorf("no record should be inserted at cap, got %d", len(repo.records))
	}
	if tx.calls != 0 {
		t.Errorf("transport must not be called at cap, got %d calls", tx.calls)
	}
}

func TestSendContactEmail_TransportFailureCompensates(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{err: errors.New("SES timeout")}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-fail").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Dave",
		SenderEmail: "dave@acme.co",
		Message:     "Will fail",
	})
	if err != nil {
		t.Fatalf("unexpected error returned to caller: %v", err)
	}
	if ok {
		t.Error("expected ok=false when transport fails")
	}
	if reason == "" {
		t.Error("expected non-empty reason when transport fails")
	}
	// Compensating delete must have been called.
	if len(repo.deleteIDs) != 1 {
		t.Errorf("expected 1 compensating delete, got %d", len(repo.deleteIDs))
	}
}

func TestSendContactEmail_TransportFailure_ReasonHidesInternals(t *testing.T) {
	t.Parallel()
	const internalErr = "secret SES error 0xdeadbeef"
	repo := &fakeRepo{}
	tx := &fakeTransport{err: errors.New(internalErr)}
	svc := newSvc(repo, tx, 100)

	_, reason, _ := svc.Bind("sess-hide").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Eve",
		SenderEmail: "eve@acme.co",
		Message:     "Hide internals",
	})

	if strings.Contains(reason, internalErr) {
		t.Errorf("internal error detail must not appear in reason, got %q", reason)
	}
}

// --- validation tests ---

func TestSendContactEmail_EmptyName(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-v1").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "",
		SenderEmail: "a@acme.co",
		Message:     "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for empty name")
	}
	if reason == "" {
		t.Error("expected non-empty reason for empty name")
	}
	if len(repo.records) != 0 || tx.calls != 0 {
		t.Error("nothing should be recorded or sent for invalid input")
	}
}

func TestSendContactEmail_BadEmail(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-v2").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Alice",
		SenderEmail: "not-an-email",
		Message:     "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for invalid email")
	}
	if reason == "" {
		t.Error("expected non-empty reason for invalid email")
	}
	if len(repo.records) != 0 || tx.calls != 0 {
		t.Error("nothing should be recorded or sent for invalid input")
	}
}

func TestSendContactEmail_OversizeMessage(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	ok, reason, err := svc.Bind("sess-v3").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Alice",
		SenderEmail: "alice@acme.co",
		Message:     strings.Repeat("x", 5001),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for oversize message")
	}
	if reason == "" {
		t.Error("expected non-empty reason for oversize message")
	}
	if len(repo.records) != 0 || tx.calls != 0 {
		t.Error("nothing should be recorded or sent for invalid input")
	}
}

func TestSendContactEmail_EmailNormalizedToAddrSpec(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	tx := &fakeTransport{}
	svc := newSvc(repo, tx, 100)

	_, _, err := svc.Bind("sess-norm").SendContactEmail(context.Background(), email.ContactEmail{
		SenderName:  "Alice",
		SenderEmail: "Alice Liddell <alice@acme.co>",
		Message:     "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(repo.records))
	}
	if repo.records[0].SenderEmail != "alice@acme.co" {
		t.Errorf("SenderEmail not normalised: got %q, want %q", repo.records[0].SenderEmail, "alice@acme.co")
	}
}

// TestSendContactEmail_FakePlaceholderRejected covers RFC-reserved and common
// throwaway domains the model may hallucinate or a visitor may try.
func TestSendContactEmail_FakePlaceholderRejected(t *testing.T) {
	t.Parallel()
	cases := []string{
		"alice@example.com",
		"alice@example.org",
		"bob@test.com",
		"carol@foo.test",
		"dave@something.invalid",
		"eve@mailinator.com",
		"frank@password.exchange",
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{}
			tx := &fakeTransport{}
			svc := newSvc(repo, tx, 100)

			ok, reason, err := svc.Bind("sess-fake").SendContactEmail(context.Background(), email.ContactEmail{
				SenderName:  "Visitor",
				SenderEmail: addr,
				Message:     "Hello",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok {
				t.Errorf("expected ok=false for placeholder %q", addr)
			}
			if reason == "" {
				t.Errorf("expected non-empty reason for placeholder %q", addr)
			}
			if len(repo.records) != 0 || tx.calls != 0 {
				t.Errorf("nothing should be recorded or sent for placeholder %q", addr)
			}
		})
	}
}

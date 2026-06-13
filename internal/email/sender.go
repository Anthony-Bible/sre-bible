package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

// ContactEmail is a Viewer's outbound message to the Owner.
type ContactEmail struct {
	SenderName  string
	SenderEmail string
	Message     string
}

// ErrSessionAlreadySent is returned by ContactRepository.RecordSend when the
// session has already sent a contact email (unique index violation).
var ErrSessionAlreadySent = errors.New("session already sent a contact email")

// ContactRepository persists contact email records for rate-limiting and audit.
// Defined here (consumed here); implemented by *db.ContactStore.
type ContactRepository interface {
	CountSince(ctx context.Context, t time.Time) (int, error)
	// RecordSend inserts a record and returns the new row ID.
	// Returns ErrSessionAlreadySent on a unique index violation.
	RecordSend(ctx context.Context, sessionID string, e ContactEmail) (id int64, err error)
	DeleteSend(ctx context.Context, id int64) error
}

// Message is the outbound email payload handed to Transport.
type Message struct {
	From    string
	To      string
	ReplyTo string
	Subject string
	Body    string
}

// Transport delivers a single outbound email message.
// Defined here (consumed here); implemented by *SESTransport.
type Transport interface {
	Send(ctx context.Context, msg Message) error
}

// Config holds the runtime parameters for the email service.
type Config struct {
	From        string
	To          string
	GlobalLimit int
	Window      time.Duration
}

// Service enforces rate-limiting, records sends, and delivers contact emails.
type Service struct {
	repo ContactRepository
	tx   Transport
	cfg  Config
	log  *slog.Logger
}

// NewService creates a Service.
func NewService(repo ContactRepository, tx Transport, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, tx: tx, cfg: cfg, log: log}
}

// Bind returns a session-bound sender that structurally satisfies rag.EmailSender.
func (s *Service) Bind(sessionID string) *BoundSender {
	return &BoundSender{svc: s, sessionID: sessionID}
}

// BoundSender is a session-scoped email sender returned by Service.Bind.
type BoundSender struct {
	svc       *Service
	sessionID string
}

// SendContactEmail delivers a contact email for the bound session.
func (b *BoundSender) SendContactEmail(ctx context.Context, e ContactEmail) (bool, string, error) {
	return b.svc.sendContactEmail(ctx, b.sessionID, e)
}

func (s *Service) sendContactEmail(ctx context.Context, sessionID string, e ContactEmail) (bool, string, error) {
	// 1. Validate inputs.
	e.SenderName = strings.TrimSpace(e.SenderName)
	e.SenderEmail = strings.TrimSpace(e.SenderEmail)
	e.Message = strings.TrimSpace(e.Message)

	if e.SenderName == "" {
		return false, "Please provide your name so Anthony knows who is reaching out.", nil
	}
	if len(e.SenderName) > 200 {
		return false, "Your name is too long — please keep it under 200 characters.", nil
	}
	// mail.ParseAddress returns nil addr on invalid input; use addr.Address to
	// normalise away any display-name form ("Alice <a@b.com>" → "a@b.com").
	parsed, _ := mail.ParseAddress(e.SenderEmail)
	if parsed == nil {
		return false, "Please provide a valid email address so Anthony can reply to you.", nil
	}
	e.SenderEmail = parsed.Address
	if isObviouslyFakeEmail(e.SenderEmail) {
		return false, "That email address looks like a placeholder. Please provide a real address so Anthony can reply to you.", nil
	}
	if e.Message == "" {
		return false, "Please include a message to send to Anthony.", nil
	}
	if len(e.Message) > 5000 {
		return false, "Your message is too long — please keep it under 5000 characters.", nil
	}

	// 2. Global hourly cap (COUNT-then-INSERT may overshoot by ≤ replica count — acceptable for abuse protection).
	count, err := s.repo.CountSince(ctx, time.Now().Add(-s.cfg.Window))
	if err != nil {
		s.log.ErrorContext(ctx, "count contact emails", slog.Any("err", err))
		return false, linkedInFallback("Unable to send your message right now."), nil
	}
	if count >= s.cfg.GlobalLimit {
		return false, linkedInFallback("Anthony's inbox is currently at capacity."), nil
	}

	// 3. Record the send intent (unique index enforces 1-per-session).
	id, err := s.repo.RecordSend(ctx, sessionID, e)
	if errors.Is(err, ErrSessionAlreadySent) {
		return false, linkedInFallback("A message has already been sent to Anthony in this conversation."), nil
	}
	if err != nil {
		s.log.ErrorContext(ctx, "record contact email send", slog.Any("err", err))
		return false, linkedInFallback("Unable to send your message right now."), nil
	}

	// 4. Deliver via transport; compensate on failure.
	body := fmt.Sprintf("Name: %s\nEmail: %s\n\n%s\n\n---\nSession: %s", e.SenderName, e.SenderEmail, e.Message, sessionID)
	msg := Message{
		From:    s.cfg.From,
		To:      s.cfg.To,
		ReplyTo: e.SenderEmail,
		Subject: fmt.Sprintf("New contact via sre.bible from %s", e.SenderName),
		Body:    body,
	}
	if err := s.tx.Send(ctx, msg); err != nil {
		s.log.ErrorContext(ctx, "send contact email via transport", slog.Any("err", err))
		if delErr := s.repo.DeleteSend(ctx, id); delErr != nil {
			s.log.ErrorContext(ctx, "compensating delete failed", slog.Any("err", delErr), slog.Int64("id", id))
		}
		return false, linkedInFallback("Your message could not be delivered."), nil
	}

	s.log.InfoContext(ctx, "contact email sent",
		slog.String("session", sessionID),
		slog.String("sender", e.SenderEmail),
	)
	return true, "", nil
}

// fakeEmailDomains catches obvious placeholders the model (or a careless visitor)
// might try. Covers RFC 2606 / 6761 reserved names plus a couple of common
// throwaways.
var fakeEmailDomains = map[string]struct{}{
	"example.com":       {},
	"example.org":       {},
	"example.net":       {},
	"test.com":          {},
	"email.com":         {},
	"domain.com":        {},
	"mailinator.com":    {},
	"guerrillamail.com": {},
	"password.exchange": {},
	"yopmail.com":       {},
	"10minutemail.com":  {},
	"trashmail.com":     {},
	"sharklasers.com":   {},
}

// fakeEmailTLDs catches reserved or special-use TLDs that can never receive mail.
var fakeEmailTLDs = map[string]struct{}{
	"test":      {},
	"example":   {},
	"invalid":   {},
	"localhost": {},
	"local":     {},
}

func isObviouslyFakeEmail(addr string) bool {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return true
	}
	domain := strings.ToLower(addr[at+1:])
	if _, ok := fakeEmailDomains[domain]; ok {
		return true
	}
	if dot := strings.LastIndex(domain, "."); dot >= 0 {
		if _, ok := fakeEmailTLDs[domain[dot+1:]]; ok {
			return true
		}
	}
	return false
}

func linkedInFallback(prefix string) string {
	return prefix + " You can reach Anthony directly at linkedin.com/in/anthonybible/ instead."
}

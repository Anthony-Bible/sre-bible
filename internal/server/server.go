package server

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
	"github.com/Anthony-Bible/sre-bible/internal/ratelimit"
)

//go:embed templates
var templateFS embed.FS

// Answerer is the port for streaming answers. Satisfied by *rag.Pipeline.
type Answerer interface {
	Answer(ctx context.Context, sessionID string, history []rag.Message, question string, onToken func(string) error, onTrace func(rag.TraceStep) error) ([]string, error)
}

// Suggester is the port for inactivity-triggered follow-up question cards. Satisfied
// by *rag.Pipeline. Kept separate from Answerer so the endpoint can stay disabled
// (cards simply never render) for any pipeline that does not implement it — NewServer
// type-asserts the existing Answerer rather than widening its signature.
type Suggester interface {
	SuggestFollowUps(ctx context.Context, history []rag.Message) ([]string, error)
}

// Pinger is the port for database liveness checks. Satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StoredMessage is a Message with its persisted citation list and Agent Trace,
// used for page rendering. Trace is nil for user turns and legacy assistant rows
// (NULL trace column); non-nil for assistant turns recorded with a trace.
type StoredMessage struct {
	rag.Message

	Citations []string
	Trace     []rag.TraceStep
}

// SessionState is a one-shot snapshot of the per-session flags read on every chat turn,
// fetched in a single SELECT to avoid three separate single-column reads of the same row.
// A missing session yields the zero value (all false / nil), mirroring the per-method
// "unknown session → default" contract of IsDeadpoolMode / IsSessionVerified / IsInterviewActive.
type SessionState struct {
	Verified        bool
	DeadpoolMode    bool
	InterviewActive bool
	InterviewState  *rag.InterviewState
}

// SessionRepository is the persistence port for anonymous chat sessions.
// Defined here (consumed here); implemented by *db.SessionStore.
// Compile-time assertion lives in cmd/server/main.go to avoid import cycles.
type SessionRepository interface {
	CreateSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error)
	AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string, trace []rag.TraceStep) error
	IsSessionVerified(ctx context.Context, sessionID string) (bool, error)
	MarkSessionVerified(ctx context.Context, sessionID string) error
	SetDeadpoolMode(ctx context.Context, sessionID string, enabled bool) error
	IsDeadpoolMode(ctx context.Context, sessionID string) (bool, error)
	GetInterviewState(ctx context.Context, sessionID string) (*rag.InterviewState, error)
	SetInterviewState(ctx context.Context, sessionID string, state *rag.InterviewState) error
	ClearInterviewState(ctx context.Context, sessionID string) error
	IsInterviewActive(ctx context.Context, sessionID string) (bool, error)
	GetSessionState(ctx context.Context, sessionID string) (SessionState, error)
}

// TurnstileVerifier is the port for verifying Cloudflare Turnstile tokens.
// Defined here (consumed here); implemented by *turnstile.Verifier.
type TurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) (bool, error)
}

// Server wires together the RAG pipeline and session store behind HTTP handlers.
type Server struct {
	pipeline         Answerer
	suggester        Suggester
	sessions         SessionRepository
	pinger           Pinger
	turnstile        TurnstileVerifier
	turnstileSiteKey string
	suggestLimiter   *ratelimit.Limiter
	chatLimiter      *ratelimit.Limiter
	templates        *template.Template
	log              *slog.Logger
	mux              *http.ServeMux
	handler          http.Handler
}

// defaultSuggestedQuestions returns the prompts shown on first load when there
// is no session history. Returned as a function to satisfy gochecknoglobals.
func defaultSuggestedQuestions() []string {
	return []string{
		"What are Anthony's biggest reliability wins?",
		"How does Anthony approach on-call culture and incident response?",
		"Paste a job description and I'll show how Anthony matches it",
		"What is Anthony looking for in his next role?",
		"I'd like to get in touch with Anthony",
	}
}

// NewServer creates a Server, parses embedded templates, and registers routes.
// turnstile may be nil only in tests; in production main.go always provides one.
// suggestLimiter throttles POST /suggestions and chatLimiter throttles POST
// /chat; the two endpoints have independent budgets. A nil limiter disables
// throttling for that endpoint (tests, local dev).
func NewServer(pipeline Answerer, sessions SessionRepository, pinger Pinger, turnstile TurnstileVerifier, turnstileSiteKey string, suggestLimiter *ratelimit.Limiter, chatLimiter *ratelimit.Limiter, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	t, err := template.New("").ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	mux := http.NewServeMux()
	s := &Server{
		pipeline:         pipeline,
		sessions:         sessions,
		pinger:           pinger,
		turnstile:        turnstile,
		turnstileSiteKey: turnstileSiteKey,
		suggestLimiter:   suggestLimiter,
		chatLimiter:      chatLimiter,
		templates:        t,
		log:              log,
		mux:              mux,
	}
	// Enable follow-up suggestions only when the pipeline implements the port. Test
	// fakes that don't simply leave POST /suggestions returning an empty card list.
	if sg, ok := pipeline.(Suggester); ok {
		s.suggester = sg
	}

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /messages", s.handleMessages)
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("POST /suggestions", s.handleSuggestions)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	s.handler = metricsMiddleware(mux)
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

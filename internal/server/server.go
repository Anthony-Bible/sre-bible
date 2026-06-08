package server

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

//go:embed templates
var templateFS embed.FS

// Answerer is the port for streaming answers. Satisfied by *rag.Pipeline.
type Answerer interface {
	Answer(ctx context.Context, sessionID string, history []rag.Message, question string, onToken func(string) error, onTrace func(rag.TraceStep) error) ([]string, error)
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

// SessionRepository is the persistence port for anonymous chat sessions.
// Defined here (consumed here); implemented by *db.SessionStore.
// Compile-time assertion lives in cmd/server/main.go to avoid import cycles.
type SessionRepository interface {
	CreateSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error)
	AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string, trace []rag.TraceStep) error
	IsSessionVerified(ctx context.Context, sessionID string) (bool, error)
	MarkSessionVerified(ctx context.Context, sessionID string) error
}

// TurnstileVerifier is the port for verifying Cloudflare Turnstile tokens.
// Defined here (consumed here); implemented by *turnstile.Verifier.
type TurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) (bool, error)
}

// Server wires together the RAG pipeline and session store behind HTTP handlers.
type Server struct {
	pipeline         Answerer
	sessions         SessionRepository
	pinger           Pinger
	turnstile        TurnstileVerifier
	turnstileSiteKey string
	templates        *template.Template
	log              *slog.Logger
	mux              *http.ServeMux
}

// defaultSuggestedQuestions returns the prompts shown on first load when there
// is no session history. Returned as a function to satisfy gochecknoglobals.
func defaultSuggestedQuestions() []string {
	return []string{
		"What are Anthony's biggest reliability wins?",
		"How does Anthony approach on-call culture and incident response?",
		"What infrastructure tooling and cloud platforms has Anthony used?",
		"What is Anthony looking for in his next role?",
		"I'd like to get in touch with Anthony",
	}
}

// NewServer creates a Server, parses embedded templates, and registers routes.
// turnstile may be nil only in tests; in production main.go always provides one.
func NewServer(pipeline Answerer, sessions SessionRepository, pinger Pinger, turnstile TurnstileVerifier, turnstileSiteKey string, log *slog.Logger) (*Server, error) {
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
		templates:        t,
		log:              log,
		mux:              mux,
	}

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /messages", s.handleMessages)
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

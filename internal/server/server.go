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
	Answer(ctx context.Context, history []rag.Message, question string, onToken func(string) error) ([]string, error)
}

// Pinger is the port for database liveness checks. Satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StoredMessage is a Message with its persisted citation list, used for page rendering.
type StoredMessage struct {
	rag.Message

	Citations []string
}

// SessionRepository is the persistence port for anonymous chat sessions.
// Defined here (consumed here); implemented by *db.SessionStore.
// Compile-time assertion lives in cmd/server/main.go to avoid import cycles.
type SessionRepository interface {
	CreateSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error)
	AppendMessage(ctx context.Context, sessionID string, msg rag.Message, citations []string) error
}

// Server wires together the RAG pipeline and session store behind HTTP handlers.
type Server struct {
	pipeline  Answerer
	sessions  SessionRepository
	pinger    Pinger
	templates *template.Template
	log       *slog.Logger
	mux       *http.ServeMux
}

// defaultSuggestedQuestions returns the prompts shown on first load when there
// is no session history. Returned as a function to satisfy gochecknoglobals.
func defaultSuggestedQuestions() []string {
	return []string{
		"What is Anthony's experience with Kubernetes and GKE?",
		"How has Anthony approached platform reliability at scale?",
		"What does Anthony's incident management background look like?",
		"What SRE practices has Anthony championed in past roles?",
	}
}

// NewServer creates a Server, parses embedded templates, and registers routes.
func NewServer(pipeline Answerer, sessions SessionRepository, pinger Pinger, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	t, err := template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	mux := http.NewServeMux()
	s := &Server{
		pipeline:  pipeline,
		sessions:  sessions,
		pinger:    pinger,
		templates: t,
		log:       log,
		mux:       mux,
	}

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

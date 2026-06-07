package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// handleHealthz is the liveness probe endpoint. Always returns 200.
// A DB outage must not cause a crash-loop restart, so this never calls the pinger.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadyz is the readiness probe endpoint. Checks DB reachability via s.pinger.
// Returns 503 if pinger is nil or if Ping fails.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.pinger == nil {
		http.Error(w, `{"status":"no pinger"}`, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pinger.Ping(ctx); err != nil {
		s.log.ErrorContext(ctx, "readyz ping failed", slog.Any("err", err))
		http.Error(w, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

type chatData struct {
	SuggestedQuestions []string
}

// messageDTO is the JSON shape returned by GET /messages.
type messageDTO struct {
	Role      string   `json:"role"`
	Content   string   `json:"content"`
	Citations []string `json:"citations"`
}

// handleIndex renders the chat shell. History is loaded client-side via GET /messages.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := chatData{
		SuggestedQuestions: defaultSuggestedQuestions(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.ErrorContext(r.Context(), "execute template", slog.Any("err", err))
	}
}

// handleMessages returns the message history for a session as JSON.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sid, ok := requireSession(w, r)
	if !ok {
		return
	}

	stored, err := s.sessions.ListMessages(r.Context(), sid)
	if err != nil {
		s.log.ErrorContext(r.Context(), "list messages", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	dtos := make([]messageDTO, len(stored))
	for i, sm := range stored {
		citations := sm.Citations
		if citations == nil {
			citations = []string{}
		}
		dtos[i] = messageDTO{
			Role:      string(sm.Role),
			Content:   sm.Content,
			Citations: citations,
		}
	}

	b, err := json.Marshal(dtos)
	if err != nil {
		s.log.ErrorContext(r.Context(), "encode messages", slog.Any("err", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// handleChat accepts a question via POST form, streams the RAG answer as SSE,
// and persists both turns to the session.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sid, ok := requireSession(w, r)
	if !ok {
		return
	}

	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" {
		http.Error(w, "question required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	if err := s.sessions.CreateSession(ctx, sid); err != nil {
		s.log.ErrorContext(ctx, "create session", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to initialise session")
		return
	}

	stored, err := s.sessions.ListMessages(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "list messages", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to load history")
		return
	}

	history := make([]rag.Message, len(stored))
	for i, sm := range stored {
		history[i] = sm.Message
	}

	if err := s.sessions.AppendMessage(ctx, sid, rag.Message{Role: rag.RoleUser, Content: question}, nil); err != nil {
		s.log.ErrorContext(ctx, "append user message", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to save message")
		return
	}

	var buf strings.Builder
	onStatus := func(msg string) error {
		return sseStatus(w, flusher, msg)
	}
	citations, err := s.pipeline.Answer(ctx, sid, history, question, func(tok string) error {
		buf.WriteString(tok)
		return sseToken(w, flusher, tok)
	}, onStatus)
	if err != nil {
		s.log.ErrorContext(ctx, "pipeline answer", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to generate response")
		return
	}

	// Use a detached context so the DB write survives a browser disconnect.
	persistCtx := context.WithoutCancel(ctx)
	if err := s.sessions.AppendMessage(
		persistCtx,
		sid,
		rag.Message{Role: rag.RoleAssistant, Content: buf.String()},
		citations,
	); err != nil {
		s.log.ErrorContext(persistCtx, "append assistant message", slog.Any("err", err), slog.String("session", sid))
	}

	_ = sseDone(w, flusher, citations)
}

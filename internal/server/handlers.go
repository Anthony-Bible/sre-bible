package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

type chatData struct {
	Messages           []renderedMessage
	ShowSuggestions    bool
	SuggestedQuestions []string
}

type renderedMessage struct {
	Role      string
	Content   string
	Citations []string
}

// handleIndex renders the chat page, loading session history from the cookie.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sid := sessionFromRequest(r)
	if sid == "" {
		id, err := newSessionID()
		if err != nil {
			s.log.ErrorContext(r.Context(), "generate session ID", slog.Any("err", err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sid = id
		setSessionCookie(w, sid)
	}

	stored, err := s.sessions.ListMessages(r.Context(), sid)
	if err != nil {
		s.log.ErrorContext(r.Context(), "list messages", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	msgs := make([]renderedMessage, len(stored))
	for i, sm := range stored {
		msgs[i] = renderedMessage{
			Role:      string(sm.Role),
			Content:   sm.Content,
			Citations: sm.Citations,
		}
	}

	data := chatData{
		Messages:           msgs,
		ShowSuggestions:    len(stored) == 0,
		SuggestedQuestions: suggestedQuestions,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.ErrorContext(r.Context(), "execute template", slog.Any("err", err))
	}
}

// handleChat accepts a question via POST form, streams the RAG answer as SSE,
// and persists both turns to the session.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sid := sessionFromRequest(r)
	if sid == "" {
		id, err := newSessionID()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sid = id
		setSessionCookie(w, sid)
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
	citations, err := s.pipeline.Answer(ctx, history, question, func(tok string) error {
		buf.WriteString(tok)
		return sseToken(w, flusher, tok)
	})
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

package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
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
	TurnstileSiteKey   string
}

// messageDTO is the JSON shape returned by GET /messages.
type messageDTO struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Citations []string        `json:"citations"`
	Trace     []rag.TraceStep `json:"trace"`
}

// handleIndex renders the chat shell. History is loaded client-side via GET /messages.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := chatData{
		SuggestedQuestions: defaultSuggestedQuestions(),
		TurnstileSiteKey:   s.turnstileSiteKey,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.ErrorContext(r.Context(), "execute template", slog.Any("err", err))
	}
}

// resolvePersonaMode extracts the Deadpool Mode header/query preference, updates the DB if needed, and returns the updated context carrying the PersonaMode.
func (s *Server) resolvePersonaMode(ctx context.Context, sid string, r *http.Request) context.Context {
	isDeadpool, err := s.sessions.IsDeadpoolMode(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "check deadpool mode", slog.Any("err", err), slog.String("session", sid))
	}

	headerVal := r.Header.Get("X-Deadpool-Mode")
	queryVal := r.URL.Query().Get("mode")

	shouldBeDeadpool := isDeadpool
	hasPreference := false
	if headerVal == "true" || queryVal == "deadpool" {
		shouldBeDeadpool = true
		hasPreference = true
	} else if headerVal == "false" || queryVal == "normal" {
		shouldBeDeadpool = false
		hasPreference = true
	}

	if hasPreference && shouldBeDeadpool != isDeadpool {
		if err := s.sessions.SetDeadpoolMode(ctx, sid, shouldBeDeadpool); err != nil {
			s.log.ErrorContext(ctx, "failed to update deadpool mode in DB", slog.Any("err", err), slog.Bool("enabled", shouldBeDeadpool))
		}
		isDeadpool = shouldBeDeadpool
	}

	if isDeadpool {
		return rag.WithPersonaMode(ctx, rag.ModeDeadpool)
	}
	return rag.WithPersonaMode(ctx, rag.ModeStandard)
}

// handleMessages returns the message history for a session as JSON.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sid, ok := requireSession(w, r)
	if !ok {
		return
	}

	ctx := s.resolvePersonaMode(r.Context(), sid, r)

	stored, err := s.sessions.ListMessages(ctx, sid)
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
		trace := sm.Trace
		if trace == nil {
			trace = []rag.TraceStep{}
		}
		dtos[i] = messageDTO{
			Role:      string(sm.Role),
			Content:   sm.Content,
			Citations: citations,
			Trace:     trace,
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

	ctx := s.resolvePersonaMode(r.Context(), sid, r)

	if err := s.sessions.CreateSession(ctx, sid); err != nil {
		s.log.ErrorContext(ctx, "create session", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "failed to initialise session", http.StatusInternalServerError)
		return
	}

	// Turnstile gate — skipped when no verifier is configured (tests, local dev without keys).
	if s.turnstile != nil {
		verified, err := s.sessions.IsSessionVerified(ctx, sid)
		if err != nil {
			s.log.ErrorContext(ctx, "check session verified", slog.Any("err", err), slog.String("session", sid))
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		if !verified {
			token := strings.TrimSpace(r.FormValue("cf-turnstile-response"))
			if token == "" {
				http.Error(w, "verification required", http.StatusForbidden)
				return
			}
			remoteIP := r.Header.Get("Cf-Connecting-Ip")
			if remoteIP == "" {
				remoteIP = r.RemoteAddr
			}
			tokenOK, verifyErr := s.turnstile.Verify(ctx, token, remoteIP)
			outcome := "pass"
			switch {
			case verifyErr != nil:
				outcome = "error"
			case !tokenOK:
				outcome = "fail"
			}
			metrics.M.TurnstileChecks.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("outcome", outcome)))
			if verifyErr != nil || !tokenOK {
				s.log.InfoContext(ctx, "turnstile verification failed",
					slog.Any("err", verifyErr),
					slog.String("session", sid),
				)
				http.Error(w, "verification failed", http.StatusForbidden)
				return
			}
			if err := s.sessions.MarkSessionVerified(ctx, sid); err != nil {
				s.log.ErrorContext(ctx, "mark session verified", slog.Any("err", err), slog.String("session", sid))
				// Log-and-continue; do not block the answer.
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
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

	if err := s.sessions.AppendMessage(ctx, sid, rag.Message{Role: rag.RoleUser, Content: question}, nil, nil); err != nil {
		s.log.ErrorContext(ctx, "append user message", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to save message")
		return
	}

	var buf strings.Builder
	// Accumulate the trace for persistence AND forward each step live via SSE. The
	// append happens before the write so a failed (disconnected) write still leaves the
	// step persisted with the assistant turn.
	var trace []rag.TraceStep
	onTrace := func(step rag.TraceStep) error {
		trace = append(trace, step)
		return sseTrace(w, flusher, step)
	}
	citations, err := s.pipeline.Answer(ctx, sid, history, question, func(tok string) error {
		buf.WriteString(tok)
		return sseToken(w, flusher, tok)
	}, onTrace)
	if err != nil {
		// A Model Armor block is an expected, user-relayable outcome — surface a
		// friendly refusal rather than the generic failure copy. The gate runs before
		// any token streams, so a clean error frame is emitted with nothing half-rendered.
		if errors.Is(err, rag.ErrPromptBlocked) {
			_ = sseError(w, flusher, "I can't help with that request.")
			return
		}
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
		trace,
	); err != nil {
		s.log.ErrorContext(persistCtx, "append assistant message", slog.Any("err", err), slog.String("session", sid))
	}

	_ = sseDone(w, flusher, citations)
}

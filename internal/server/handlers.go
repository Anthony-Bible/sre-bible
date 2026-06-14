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

// Deadpool Mode toggle values carried by the X-Deadpool-Mode request header.
const (
	deadpoolHeaderOn  = "true"
	deadpoolHeaderOff = "false"
)

// Interview Mode plumbing carried by request headers/params, mirroring the
// Deadpool toggle. X-Interview-Mode flips the mode on/off, X-Interview-Reset
// restarts a run from scratch. The frontend maps the /interview and /exit slash
// commands onto these.
const (
	interviewHeader      = "X-Interview-Mode"
	interviewResetHeader = "X-Interview-Reset"
	interviewHeaderOn    = "true"
	interviewHeaderOff   = "false"
)

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
	if headerVal == deadpoolHeaderOn || queryVal == "deadpool" {
		shouldBeDeadpool = true
		hasPreference = true
	} else if headerVal == deadpoolHeaderOff || queryVal == "normal" {
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

// resolveInterviewMode reads the interview header/query/reset preference, updates the
// persisted InterviewState as needed, and returns the (possibly flagged) context plus
// whether interview mode is active for this turn. It mirrors resolvePersonaMode, and
// must be called after CreateSession so SetInterviewState has a session row to write.
// DB errors are logged and treated as non-fatal — a state-store hiccup must not 500 the
// chat turn.
func (s *Server) resolveInterviewMode(ctx context.Context, sid string, r *http.Request) (context.Context, bool) {
	active, err := s.sessions.IsInterviewActive(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "check interview active", slog.Any("err", err), slog.String("session", sid))
	}

	headerVal := r.Header.Get(interviewHeader)
	queryVal := r.URL.Query().Get("mode")
	reset := r.Header.Get(interviewResetHeader) == interviewHeaderOn

	switch {
	case reset:
		// Restart: wipe any prior progress and seed a fresh run. Stays in interview mode.
		if err := s.sessions.ClearInterviewState(ctx, sid); err != nil {
			s.log.ErrorContext(ctx, "clear interview state (reset)", slog.Any("err", err), slog.String("session", sid))
		}
		if err := s.sessions.SetInterviewState(ctx, sid, rag.NewInterviewState()); err != nil {
			s.log.ErrorContext(ctx, "seed interview state (reset)", slog.Any("err", err), slog.String("session", sid))
		}
		return rag.WithInterviewMode(ctx, true), true

	case headerVal == interviewHeaderOff:
		// Explicit /exit: leave interview mode and continue as standard RAG.
		if active {
			if err := s.sessions.ClearInterviewState(ctx, sid); err != nil {
				s.log.ErrorContext(ctx, "clear interview state (exit)", slog.Any("err", err), slog.String("session", sid))
			}
		}
		return ctx, false

	case headerVal == interviewHeaderOn || queryVal == "interview":
		// First flip-on seeds the scenarios; subsequent turns just stay active.
		if !active {
			if err := s.sessions.SetInterviewState(ctx, sid, rag.NewInterviewState()); err != nil {
				s.log.ErrorContext(ctx, "seed interview state", slog.Any("err", err), slog.String("session", sid))
			}
		}
		return rag.WithInterviewMode(ctx, true), true

	default:
		// No signal: a mid-run session stays in interview mode across turns.
		if active {
			return rag.WithInterviewMode(ctx, true), true
		}
		return ctx, false
	}
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

// suggestionsDTO is the JSON shape returned by POST /suggestions. Questions is always a
// non-nil array so the client iterates without a null check.
type suggestionsDTO struct {
	Questions []string `json:"questions"`
}

// writeSuggestions writes a 200 JSON {"questions":[...]} body, normalising nil to an
// empty array. Encode errors are intentionally swallowed: a failed suggestion response
// must never surface as an error in the UI (silent no-cards).
func writeSuggestions(w http.ResponseWriter, questions []string) {
	if questions == nil {
		questions = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(suggestionsDTO{Questions: questions})
}

// handleSuggestions returns up to rag.MaxFollowUps LLM-generated follow-up questions for
// the session, grounded in its recent history and the document catalog. It is the lazy,
// inactivity-triggered backend for the suggestion cards. Every failure degrades silently
// to an empty list with HTTP 200 — a missing suggestion must never surface as an error in
// the UI. The one hard gate is abuse: an unverified session (when Turnstile is configured)
// gets 403 with no LLM call.
func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	sid, ok := requireSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Abuse gate: require the session to have already passed Turnstile via /chat.
	// Skipped when no verifier is configured (tests, local dev without keys).
	if s.turnstile != nil {
		verified, err := s.sessions.IsSessionVerified(ctx, sid)
		if err != nil {
			s.log.ErrorContext(ctx, "check session verified", slog.Any("err", err), slog.String("session", sid))
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		if !verified {
			metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", "unverified")))
			http.Error(w, "verification required", http.StatusForbidden)
			return
		}
	}

	// Rate limit: only verified sessions reach here, so unverified abuse is already
	// rejected (above) without consuming throttle budget. A throttled request makes
	// no DB read / Model Armor / LLM call — it short-circuits with 429, which the
	// client can distinguish from the silent {"questions":[]} degrade used for
	// non-abuse failures.
	if s.suggestLimiter != nil && !s.suggestLimiter.Allow(sid) {
		metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", "throttled")))
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	// Feature disabled (pipeline does not implement Suggester): no cards.
	if s.suggester == nil {
		writeSuggestions(w, nil)
		return
	}

	stored, err := s.sessions.ListMessages(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "list messages", slog.Any("err", err), slog.String("session", sid))
		writeSuggestions(w, nil)
		return
	}
	if len(stored) == 0 {
		writeSuggestions(w, nil)
		return
	}

	questions, err := s.suggester.SuggestFollowUps(ctx, storedToHistory(stored))
	if err != nil {
		s.log.ErrorContext(ctx, "suggest follow-ups", slog.Any("err", err), slog.String("session", sid))
		writeSuggestions(w, nil)
		return
	}
	writeSuggestions(w, questions)
}

// verifyTurnstile enforces the once-per-session Cloudflare Turnstile gate. It returns
// true to proceed; on any rejection it writes the HTTP error response itself and returns
// false. A nil verifier (tests, local dev without keys) short-circuits to true. On a
// successful first check it marks the session verified — best-effort, since a persistence
// error there is logged but must not block the answer.
func (s *Server) verifyTurnstile(ctx context.Context, w http.ResponseWriter, r *http.Request, sid string) bool {
	if s.turnstile == nil {
		return true
	}
	verified, err := s.sessions.IsSessionVerified(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "check session verified", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "session error", http.StatusInternalServerError)
		return false
	}
	if verified {
		return true
	}

	token := strings.TrimSpace(r.FormValue("cf-turnstile-response"))
	if token == "" {
		http.Error(w, "verification required", http.StatusForbidden)
		return false
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
		return false
	}
	if err := s.sessions.MarkSessionVerified(ctx, sid); err != nil {
		s.log.ErrorContext(ctx, "mark session verified", slog.Any("err", err), slog.String("session", sid))
		// Log-and-continue; do not block the answer.
	}
	return true
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

	// Turnstile gate — writes its own error response and returns false on rejection.
	if !s.verifyTurnstile(ctx, w, r, sid) {
		return
	}

	// Rate limit: only verified sessions reach here. A throttled request makes no
	// DB history read / embedding / Model Armor / LLM call — it short-circuits with
	// 429 before the SSE stream starts, which the client handles like any non-200
	// from /chat. Per-session cooldown + global hourly backstop, independent of the
	// /suggestions budget.
	if s.chatLimiter != nil && !s.chatLimiter.Allow(sid) {
		metrics.M.LLMResponsesBlocked.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("reason", "rate_limited")))
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	// Interview mode: resolved after the session exists and the abuse gates pass, so a
	// fresh InterviewState is only seeded for verified, non-throttled sessions.
	ctx, interview := s.resolveInterviewMode(ctx, sid, r)

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

	s.streamAnswer(ctx, w, flusher, sid, question, storedToHistory(stored), interview)
}

// storedToHistory projects stored messages down to the rag.Message turns the pipeline
// consumes, dropping the per-message citation/trace metadata the chat path doesn't replay.
func storedToHistory(stored []StoredMessage) []rag.Message {
	history := make([]rag.Message, len(stored))
	for i, sm := range stored {
		history[i] = sm.Message
	}
	return history
}

// streamAnswer persists the user turn, runs the RAG pipeline while streaming tokens and
// trace steps over SSE, then persists the assistant turn. It writes all SSE frames
// (token/trace/error/done) itself. A Model Armor block surfaces as a friendly refusal
// rather than the generic failure copy. The assistant turn is persisted under a detached
// context so the DB write survives a browser disconnect.
func (s *Server) streamAnswer(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sid, question string, history []rag.Message, interview bool) {
	if err := s.sessions.AppendMessage(ctx, sid, rag.Message{Role: rag.RoleUser, Content: question}, nil, nil); err != nil {
		s.log.ErrorContext(ctx, "append user message", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to save message")
		return
	}

	// Interview HUD progress: track how many scenarios have been graded so we can emit
	// an interview_progress event after each grading tool-call and persist the advance.
	graded, total := 0, rag.InterviewNumScenarios
	if interview {
		if state, err := s.sessions.GetInterviewState(ctx, sid); err != nil {
			s.log.ErrorContext(ctx, "get interview state", slog.Any("err", err), slog.String("session", sid))
		} else if state != nil {
			graded, total = state.CurrentQuestionIndex, state.TotalQuestions
		}
	}

	var buf strings.Builder
	// Accumulate the trace for persistence AND forward each step live via SSE. The
	// append happens before the write so a failed (disconnected) write still leaves the
	// step persisted with the assistant turn.
	var trace []rag.TraceStep
	onTrace := func(step rag.TraceStep) error {
		trace = append(trace, step)
		// In interview mode, a successful evaluate_interview_answer tool-call means one
		// more scenario has been graded: advance the HUD counter and push it live.
		if interview && step.Kind == rag.TraceKindToolCall && step.ToolCall != nil &&
			step.ToolCall.Tool == rag.ToolEvaluateInterviewAnswer && step.ToolCall.Outcome == "ok" {
			if graded < total {
				graded++
			}
			if err := sseInterviewProgress(w, flusher, graded, total); err != nil {
				return err
			}
		}
		return sseTrace(w, flusher, step)
	}
	citations, err := s.pipeline.Answer(ctx, sid, history, question, func(tok string) error {
		buf.WriteString(tok)
		return sseToken(w, flusher, tok)
	}, onTrace)
	if err != nil {
		// The gate runs before any token streams, so a clean error frame is emitted with
		// nothing half-rendered.
		if errors.Is(err, rag.ErrPromptBlocked) {
			_ = sseError(w, flusher, "I can't help with that request.")
			return
		}
		s.log.ErrorContext(ctx, "pipeline answer", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to generate response")
		return
	}

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

	// Persist the advanced scenario counter so the HUD survives a reload mid-run. Only
	// write when an answer was actually graded this turn. Detached ctx, like the turn
	// above, so a browser disconnect doesn't drop the progress.
	if interview && graded > 0 {
		if state, err := s.sessions.GetInterviewState(persistCtx, sid); err != nil {
			s.log.ErrorContext(persistCtx, "get interview state for persist", slog.Any("err", err), slog.String("session", sid))
		} else if state != nil {
			state.CurrentQuestionIndex = graded
			state.Completed = graded >= total
			if err := s.sessions.SetInterviewState(persistCtx, sid, state); err != nil {
				s.log.ErrorContext(persistCtx, "persist interview progress", slog.Any("err", err), slog.String("session", sid))
			}
		}
	}

	_ = sseDone(w, flusher, citations)
}

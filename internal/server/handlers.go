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
	InterviewEnabled   bool
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
		InterviewEnabled:   s.interviewEnabled,
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

// resolvePersonaMode extracts the Deadpool Mode header/query preference, updates the DB if needed, and returns the updated context carrying the PersonaMode. isDeadpool is the session's currently-persisted preference, read once by the caller from the session-state snapshot.
func (s *Server) resolvePersonaMode(ctx context.Context, sid string, r *http.Request, isDeadpool bool) context.Context {
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
// persisted InterviewState as needed, and returns the context flagged with the resulting
// interview mode (consumed downstream via rag.InterviewModeFromContext). It mirrors
// resolvePersonaMode, and must be called after CreateSession so SetInterviewState has a
// session row to write. DB errors are logged and treated as non-fatal — a state-store
// hiccup must not 500 the chat turn.
func (s *Server) resolveInterviewMode(ctx context.Context, sid string, r *http.Request, active bool) context.Context {
	// Kill-switch: when Interview Mode is disabled, never activate it regardless of
	// any X-Interview-* headers a client might send. Continue as standard RAG. Any
	// stale persisted state is simply never read into context (and unreachable: the
	// frontend hides the command, so no new state is ever seeded).
	if !s.interviewEnabled {
		return ctx
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
		return rag.WithInterviewMode(ctx, true)

	case headerVal == interviewHeaderOff:
		// Explicit /exit: leave interview mode and continue as standard RAG.
		if active {
			if err := s.sessions.ClearInterviewState(ctx, sid); err != nil {
				s.log.ErrorContext(ctx, "clear interview state (exit)", slog.Any("err", err), slog.String("session", sid))
			}
		}
		return ctx

	case headerVal == interviewHeaderOn || queryVal == "interview":
		// First flip-on seeds the scenarios; subsequent turns just stay active.
		if !active {
			if err := s.sessions.SetInterviewState(ctx, sid, rag.NewInterviewState()); err != nil {
				s.log.ErrorContext(ctx, "seed interview state", slog.Any("err", err), slog.String("session", sid))
			}
		}
		return rag.WithInterviewMode(ctx, true)

	default:
		// No signal: a mid-run session stays in interview mode across turns.
		if active {
			return rag.WithInterviewMode(ctx, true)
		}
		return ctx
	}
}

// retryAfterShedSeconds is the Retry-After hint sent with a load-shed 503, telling a
// client to back off briefly rather than hammer a saturated pool.
const retryAfterShedSeconds = "5"

// quickDBContext derives a short-deadline context for the per-request "quick" DB phase
// (session-state reads, history loads, session creation). pgxpool.Acquire honours the
// context deadline, so on a saturated pool the deadline fires while waiting for a free
// connection and the caller sheds a 503 instead of piling up. It must NOT wrap the
// long-running LLM/SSE stream, which keeps the full request context. The deadline is
// shorter than the DB-side statement_timeout (db.NewPool) so the context fires first.
func (s *Server) quickDBContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.quickDBTimeout)
}

// writeServiceUnavailable sheds a request when the DB pool is saturated: it sets a
// Retry-After hint, writes 503, and records the load-shed metric for endpoint. ctx is
// the (non-deadline) request context, used only for the metric record. Call before any
// response body/headers have been committed — for /chat that means before SSE headers.
func writeServiceUnavailable(ctx context.Context, w http.ResponseWriter, endpoint string) {
	metrics.M.DBLoadShed.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("endpoint", endpoint)))
	w.Header().Set("Retry-After", retryAfterShedSeconds)
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}

// snapshotSessionState reads the per-turn session flags in a single SELECT. On a read
// error it logs and returns the zero value (all defaults) with ok=false; callers behind
// the Turnstile gate must treat ok=false as fatal (can't confirm verification → must not
// bypass the gate), while ungated paths ignore ok and run on the defaults.
func (s *Server) snapshotSessionState(ctx context.Context, sid string) (SessionState, bool) {
	state, err := s.sessions.GetSessionState(ctx, sid)
	if err != nil {
		s.log.ErrorContext(ctx, "get session state", slog.Any("err", err), slog.String("session", sid))
		return SessionState{}, false
	}
	return state, true
}

// handleMessages returns the message history for a session as JSON.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sid, ok := requireSession(w, r)
	if !ok {
		return
	}

	// Quick DB phase: bound the per-session reads with a short deadline so a saturated
	// pool sheds 503 instead of queuing on connection acquisition. This whole handler is
	// a couple of cheap reads (no LLM), so the same deadline covers the lot.
	ctx, cancel := s.quickDBContext(r.Context())
	defer cancel()

	// One round-trip for the per-session flags, replacing the lone IsDeadpoolMode read.
	// No turnstile gate here, so a read error degrades to the zero value (non-fatal):
	// persona resolution simply starts from "not deadpool", as before — UNLESS the read
	// failed because the quick deadline fired (pool saturated), in which case shed 503.
	state, ok := s.snapshotSessionState(ctx, sid)
	if !ok && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeServiceUnavailable(r.Context(), w, "messages")
		return
	}
	ctx = s.resolvePersonaMode(ctx, sid, r, state.DeadpoolMode)

	stored, err := s.sessions.ListMessages(ctx, sid)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeServiceUnavailable(r.Context(), w, "messages")
			return
		}
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

	// Quick DB phase: the verified-gate read and the pre-LLM history read are bounded by
	// a short deadline so a saturated pool sheds 503 instead of queuing. The LLM/suggester
	// call below keeps the full request context (ctx), not this one.
	dbCtx, cancel := s.quickDBContext(ctx)
	defer cancel()

	// Abuse gate: require the session to have already passed Turnstile via /chat.
	// Skipped when no verifier is configured (tests, local dev without keys).
	if s.turnstile != nil {
		verified, err := s.sessions.IsSessionVerified(dbCtx, sid)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				writeServiceUnavailable(ctx, w, "suggestions")
				return
			}
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

	stored, err := s.sessions.ListMessages(dbCtx, sid)
	if err != nil {
		// Pool saturation: shed an explicit 503 so the client backs off, rather than the
		// silent {"questions":[]} degrade used for genuine (non-abuse, non-saturation) errors.
		if errors.Is(err, context.DeadlineExceeded) {
			writeServiceUnavailable(ctx, w, "suggestions")
			return
		}
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
func (s *Server) verifyTurnstile(ctx context.Context, w http.ResponseWriter, r *http.Request, sid string, verified bool) bool {
	if s.turnstile == nil {
		return true
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

	// Quick DB phase: the pre-stream reads/writes (session-state snapshot, CreateSession,
	// and the history load below) are bounded by a short deadline so a saturated pool
	// sheds a clean HTTP 503 before any SSE response is committed, instead of queuing on
	// connection acquisition. The persona/interview-derived ctx handed to the stream keeps
	// the full request context (r.Context()), so the LLM path is unaffected.
	dbCtx, cancel := s.quickDBContext(r.Context())
	defer cancel()

	// Snapshot the per-session flags in a single SELECT, replacing what used to be three
	// separate single-column reads of the same row (deadpool / verified / interview-active).
	// Read before resolvePersonaMode/CreateSession to preserve the existing ordering.
	state, ok := s.snapshotSessionState(dbCtx, sid)
	if !ok {
		// A fired quick deadline means the pool is saturated → shed 503 (takes priority
		// over the gate-safety 500 below).
		if errors.Is(dbCtx.Err(), context.DeadlineExceeded) {
			writeServiceUnavailable(r.Context(), w, "chat")
			return
		}
		if s.turnstile != nil {
			// Read failed and we can't confirm verification → must not bypass the gate.
			// Mirrors the old verifyTurnstile 500-on-read-error behavior. With no turnstile
			// (tests/local) the error is non-fatal and we run on the zero-value defaults.
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
	}

	ctx := s.resolvePersonaMode(r.Context(), sid, r, state.DeadpoolMode)

	if err := s.sessions.CreateSession(dbCtx, sid); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeServiceUnavailable(r.Context(), w, "chat")
			return
		}
		s.log.ErrorContext(ctx, "create session", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "failed to initialise session", http.StatusInternalServerError)
		return
	}

	// Turnstile gate — writes its own error response and returns false on rejection.
	if !s.verifyTurnstile(ctx, w, r, sid, state.Verified) {
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
	// fresh InterviewState is only seeded for verified, non-throttled sessions. The
	// resulting mode rides the context; streamAnswer reads it back off ctx.
	ctx = s.resolveInterviewMode(ctx, sid, r, state.InterviewActive)

	// Load history under the quick deadline BEFORE committing to an SSE response, so a
	// saturated pool sheds a clean HTTP 503 rather than a 200 stream with an error frame.
	// This is the last pre-stream DB op; everything after it streams.
	stored, err := s.sessions.ListMessages(dbCtx, sid)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeServiceUnavailable(r.Context(), w, "chat")
			return
		}
		s.log.ErrorContext(ctx, "list messages", slog.Any("err", err), slog.String("session", sid))
		http.Error(w, "failed to load history", http.StatusInternalServerError)
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

	s.streamAnswer(ctx, w, flusher, sid, question, storedToHistory(stored))
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
func (s *Server) streamAnswer(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sid, question string, history []rag.Message) {
	if err := s.sessions.AppendMessage(ctx, sid, rag.Message{Role: rag.RoleUser, Content: question}, nil, nil); err != nil {
		s.log.ErrorContext(ctx, "append user message", slog.Any("err", err), slog.String("session", sid))
		_ = sseError(w, flusher, "failed to save message")
		return
	}

	// Interview HUD progress: load the state once so we can both seed the counter and
	// persist the advance below, and track how many scenarios have been graded so we can
	// emit an interview_progress event after each grading tool-call.
	interview := rag.InterviewModeFromContext(ctx)
	var interviewState *rag.InterviewState
	graded, total := 0, rag.InterviewNumScenarios
	if interview {
		if state, err := s.sessions.GetInterviewState(ctx, sid); err != nil {
			s.log.ErrorContext(ctx, "get interview state", slog.Any("err", err), slog.String("session", sid))
		} else if state != nil {
			interviewState = state
			graded = state.CurrentQuestionIndex
			// Guard against a malformed/legacy row whose TotalQuestions is 0, which would
			// otherwise zero the cap and stall the HUD at "0 of 0".
			if state.TotalQuestions > 0 {
				total = state.TotalQuestions
			}
		}
	}
	// gradedBefore is the count carried in from prior turns; comparing against it lets us
	// persist only when this turn actually advanced the counter.
	gradedBefore := graded

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

	// Persist the advanced scenario counter so the HUD survives a reload mid-run, reusing
	// the state loaded above. Only write when this turn actually graded an answer (the
	// counter moved past gradedBefore) — a clarifying-question turn must not re-write the
	// row. Detached ctx, like the turn above, so a browser disconnect doesn't drop it.
	if graded > gradedBefore && interviewState != nil {
		interviewState.CurrentQuestionIndex = graded
		interviewState.Completed = graded >= total
		if err := s.sessions.SetInterviewState(persistCtx, sid, interviewState); err != nil {
			s.log.ErrorContext(persistCtx, "persist interview progress", slog.Any("err", err), slog.String("session", sid))
		}
	}

	_ = sseDone(w, flusher, citations)
}

package rag

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
)

const defaultK = 8

// MaxFollowUps caps how many follow-up suggestion cards a single SuggestFollowUps
// call may return.
const MaxFollowUps = 2

// maxHistoryForFollowUps bounds how many trailing conversation Messages are fed to
// the follow-up generator. Scoped to the latest exchange only (the most recent user
// question + assistant answer): suggestions must continue the CURRENT thread, and a
// weak instruction-follower given more turns drifts back to earlier topics. Also keeps
// the one-shot call cheap.
const maxHistoryForFollowUps = 2

// MaxFollowUpTokens caps the completion length of a follow-up suggestion call. A couple
// of short questions fit comfortably, and a tight cap keeps the inactivity-triggered call
// cheap. Both FollowUpSuggester implementations (Anthropic + OpenAI-compatible) share it.
const MaxFollowUpTokens = 256

// CapQuestions trims surrounding whitespace from each follow-up question, drops empty
// entries, and limits the result to at most maxQuestions. It returns nil when nothing
// survives, so a degenerate generator response surfaces as "no cards" rather than an
// empty-string button. Both FollowUpSuggester implementations funnel their parsed output
// through it, so every provider trims and caps identically.
func CapQuestions(questions []string, maxQuestions int) []string {
	if maxQuestions <= 0 {
		return nil
	}
	out := make([]string, 0, len(questions))
	for _, q := range questions {
		if q = strings.TrimSpace(q); q == "" {
			continue
		}
		out = append(out, q)
		if len(out) >= maxQuestions {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FollowUpSuggester does a single, cheap, non-streaming generation that proposes
// short follow-up questions grounded in the conversation and the document catalog.
// systemPrompt is the persona-neutral, scope-locked instruction (FollowUpSystemPrompt);
// messages is the recent history plus a final user turn carrying the catalog;
// maxQuestions caps the result. Implemented by *llm.Client and the OpenAI-compatible
// suggester (asserted in cmd/server/main.go).
type FollowUpSuggester interface {
	SuggestFollowUps(ctx context.Context, systemPrompt string, messages []Message, maxQuestions int) ([]string, error)
}

// Pipeline wires together embedding, retrieval, and generation.
type Pipeline struct {
	embedder   QueryEmbedder
	searcher   ChunkSearcher
	generator  Generator
	lister     DocumentLister
	fetcher    FullTextFetcher
	matcher    JobMatcher
	judge      Judge
	emailerFor EmailerFactory
	sanitizer  PromptSanitizer
	suggester  FollowUpSuggester
	k          int
	log        *slog.Logger
	onToolCall func(string)
}

// PipelineOption is a functional option for configuring a Pipeline.
type PipelineOption func(*Pipeline)

// WithOnToolCall returns a PipelineOption that registers a hook called with the
// tool name each time the generator invokes a tool during the agentic loop.
func WithOnToolCall(fn func(toolName string)) PipelineOption {
	return func(p *Pipeline) { p.onToolCall = fn }
}

// WithPromptSanitizer returns a PipelineOption that gates inbound questions through
// s before embedding or generation. A nil sanitizer (the default) skips the gate.
func WithPromptSanitizer(s PromptSanitizer) PipelineOption {
	return func(p *Pipeline) { p.sanitizer = s }
}

// WithFollowUpSuggester returns a PipelineOption that enables SuggestFollowUps by
// wiring in the one-shot generator. A nil suggester (the default) makes
// SuggestFollowUps a no-op (returns nil, nil), so cmd/query and tests are unaffected.
func WithFollowUpSuggester(s FollowUpSuggester) PipelineOption {
	return func(p *Pipeline) { p.suggester = s }
}

// WithJudge returns a PipelineOption that wires an interview Judge into the
// ToolSet so the model may invoke the evaluate_interview_answer tool. Passing
// an untyped-nil Judge keeps the tool unadvertised — the same gating pattern
// matcher and emailer use. (A typed-nil pointer wrapped in the interface is
// still a non-nil interface value and will advertise the tool, matching the
// existing convention for the other tool options.)
func WithJudge(j Judge) PipelineOption {
	return func(p *Pipeline) { p.judge = j }
}

// NewPipeline creates a Pipeline. Pass k=0 to use defaultK (8).
// lister and fetcher may be nil; when both are non-nil the model may invoke
// the list_documents / fetch_full_document tools to escalate beyond chunks.
// matcher may be nil; when non-nil, the match_job_description tool is advertised.
// emailerFor may be nil; when non-nil, the send_contact_email tool is advertised.
func NewPipeline(embedder QueryEmbedder, searcher ChunkSearcher, generator Generator, lister DocumentLister, fetcher FullTextFetcher, matcher JobMatcher, emailerFor EmailerFactory, k int, log *slog.Logger, opts ...PipelineOption) *Pipeline {
	if k <= 0 {
		k = defaultK
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pipeline{
		embedder:   embedder,
		searcher:   searcher,
		generator:  generator,
		lister:     lister,
		fetcher:    fetcher,
		matcher:    matcher,
		emailerFor: emailerFor,
		k:          k,
		log:        log,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Answer embeds the question, retrieves relevant chunks, assembles the full
// message history, streams a grounded response via onToken, and returns
// deduplicated citation source names.
//
// sessionID identifies the current session; used to create a session-bound
// EmailSender when an emailerFor factory is configured.
// history contains prior turns from the Session (may be empty for first turn).
// onTrace, if non-nil, receives each TraceStep in order: the retrieval step (always,
// including the zero-chunk path), then the generator's tool_call and answer steps.
// In interview mode (InterviewModeFromContext) retrieval is skipped entirely, so no
// retrieval step is emitted and the model receives the raw question with no context block.
// citations include both vector-retrieved chunk sources and any documents fetched
// via the fetch_full_document tool during generation.
func (p *Pipeline) Answer(ctx context.Context, sessionID string, history []Message, question string, onToken func(string) error, onTrace func(TraceStep) error) ([]string, error) {
	start := time.Now()
	// Inbound prompt gate: screen for jailbreak / prompt-injection before the model
	// ever sees the question. A match blocks with a typed sentinel error.
	if p.screenPrompt(ctx, question) {
		metrics.M.LLMResponsesBlocked.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("reason", "model_armor")))
		return nil, ErrPromptBlocked
	}

	// retrieve runs the embed → search → trace → zero-chunk path, or — in interview
	// mode — skips all of it and returns the raw question. done=true means it already
	// streamed the zero-chunk canned reply and recorded the served/duration metrics, so
	// Answer is finished.
	chunks, currentMsg, done, err := p.retrieve(ctx, question, onToken, onTrace, start)
	if err != nil {
		return nil, err
	}
	if done {
		return nil, nil
	}

	messages := make([]Message, len(history)+1)
	copy(messages, history)
	messages[len(history)] = currentMsg

	tools := ToolSet{Lister: p.lister, Fetcher: p.fetcher, Matcher: p.matcher, Judge: p.judge}
	if p.emailerFor != nil {
		tools.Emailer = p.emailerFor(sessionID)
	}
	traceForGen := onTrace
	if p.onToolCall != nil {
		hook := p.onToolCall
		traceForGen = func(step TraceStep) error {
			if step.Kind == TraceKindToolCall && step.ToolCall != nil {
				hook(step.ToolCall.Tool)
			}
			if onTrace != nil {
				return onTrace(step)
			}
			return nil
		}
	}
	toolFetched, err := p.generator.StreamAnswer(ctx, messages, tools, onToken, traceForGen)
	if err != nil {
		metrics.M.LLMErrors.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("stage", "stream")))
		return nil, err
	}
	metrics.M.LLMResponsesServed.Add(ctx, 1)
	metrics.M.LLMDuration.Record(ctx, time.Since(start).Seconds())

	// Citations = vector-retrieved chunk sources first (in retrieval order), then any
	// documents the model fetched via tools, all deduped by DedupeSourceNames.
	names := make([]string, 0, len(chunks)+len(toolFetched))
	for _, c := range chunks {
		names = append(names, c.SourceName)
	}
	names = append(names, toolFetched...)
	citations := DedupeSourceNames(names)

	p.log.InfoContext(ctx, "query answered", "chunks", len(chunks), "citations", len(citations))
	return citations, nil
}

// retrieve prepares the current-turn user Message for Answer.
//
// In interview mode (InterviewModeFromContext) the agent drives scripted SRE
// scenarios, not résumé retrieval, so embedding + chunk search are skipped
// entirely, no retrieval trace step is emitted, and the raw question is returned
// as the user Message with no <context> block (chunks stay nil).
//
// Otherwise it runs the normal path: embed → search → emit the retrieval trace
// step (on both the populated and zero-chunk paths) → and, when the search
// returns no chunks, stream the canned "couldn't find" reply, record the served +
// duration metrics, and report done=true so Answer returns without invoking the
// generator. done=true means the answer is already fully handled.
func (p *Pipeline) retrieve(ctx context.Context, question string, onToken func(string) error, onTrace func(TraceStep) error, start time.Time) ([]RetrievedChunk, Message, bool, error) {
	if InterviewModeFromContext(ctx) {
		return nil, Message{Role: RoleUser, Content: question}, false, nil
	}

	queryVec, err := p.embedder.EmbedQuery(ctx, question)
	if err != nil {
		metrics.M.LLMErrors.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("stage", "embed")))
		return nil, Message{}, false, err
	}

	retrievalStart := time.Now()
	chunks, err := p.searcher.SearchChunks(ctx, queryVec, p.k)
	if err != nil {
		metrics.M.LLMErrors.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("stage", "search")))
		return nil, Message{}, false, err
	}
	metrics.M.RAGRetrievalDuration.Record(ctx, time.Since(retrievalStart).Seconds())
	metrics.M.RAGChunksRetrieved.Record(ctx, int64(len(chunks)))

	// Emit the retrieval step on BOTH paths — before the zero-chunk branch — so it
	// fires even on early return. This is the only place chunk Content is available
	// before BuildUserMessage consumes it, so the grounding excerpts are captured here.
	// Trace emission is best-effort: a failed transient write must not abort the answer
	// (cancellation is handled via ctx, token streaming via onToken).
	if onTrace != nil {
		_ = onTrace(buildRetrievalStep(chunks))
	}

	if len(chunks) == 0 {
		if err := onToken("I couldn't find relevant information in my knowledge base to answer that question."); err != nil {
			return nil, Message{}, false, err
		}
		metrics.M.LLMResponsesServed.Add(ctx, 1)
		metrics.M.LLMDuration.Record(ctx, time.Since(start).Seconds())
		return nil, Message{}, true, nil
	}

	return chunks, BuildUserMessage(question, chunks), false, nil
}

// screenPrompt runs the inbound Model Armor gate on text. It returns true only when
// the sanitizer positively flagged the text as a jailbreak / prompt-injection attempt.
// A nil sanitizer (cmd/query, tests) returns false, and a sanitizer availability error
// is fail-open — returns false after logging loudly (a Model Armor outage must not take
// a feature down; see ADR 0011). The caller decides how a true verdict degrades: Answer
// refuses with ErrPromptBlocked, SuggestFollowUps silently drops the cards.
func (p *Pipeline) screenPrompt(ctx context.Context, text string) bool {
	if p.sanitizer == nil {
		return false
	}
	blocked, reason, err := p.sanitizer.SanitizePrompt(ctx, text)
	switch {
	case err != nil:
		p.log.ErrorContext(ctx, "model armor check failed; allowing (fail-open)", slog.Any("err", err))
		return false
	case blocked:
		p.log.WarnContext(ctx, "prompt blocked by model armor", slog.String("reason", reason))
		return true
	}
	return false
}

// SuggestFollowUps proposes up to MaxFollowUps short follow-up questions a visitor
// might ask next, grounded in the recent conversation and the document catalog. It is
// a cheap, one-shot, non-streaming generation used by the inactivity-triggered
// suggestion cards; it never streams and never invokes RAG tools.
//
// It returns nil (no error) when the feature is disabled (no suggester configured) or
// there is no history to build on. Before the LLM call it screens the user-authored
// turns through the Model Armor gate: a detected injection drops the cards (no error,
// status=blocked) so the suggestion path can't be turned into a free general-purpose
// chatbot; a Model Armor outage is fail-open. The conversation is otherwise treated as
// untrusted data by the hardened FollowUpSystemPrompt.
func (p *Pipeline) SuggestFollowUps(ctx context.Context, history []Message) ([]string, error) {
	if p.suggester == nil || len(history) == 0 {
		return nil, nil
	}

	// Bound the history first so the Model Armor gate screens exactly the user-authored
	// text that will reach the generator — no more, no less. Assistant prose is ours.
	tail := history[max(0, len(history)-maxHistoryForFollowUps):]
	if p.screenPrompt(ctx, recentUserText(tail)) {
		metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", "blocked")))
		return nil, nil
	}

	catalog, err := p.buildCatalog(ctx)
	if err != nil {
		metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", "error")))
		return nil, err
	}

	// messages = trailing history (bounded) + a final user turn carrying the catalog.
	messages := make([]Message, 0, len(tail)+1)
	messages = append(messages, tail...)
	messages = append(messages, Message{Role: RoleUser, Content: BuildFollowUpInstruction(catalog)})

	questions, err := p.suggester.SuggestFollowUps(ctx, FollowUpSystemPrompt, messages, MaxFollowUps)
	if err != nil {
		metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", "error")))
		return nil, err
	}

	status := "ok"
	if len(questions) == 0 {
		status = "empty"
	}
	metrics.M.FollowUpSuggestions.Add(ctx, 1, metric.WithAttributes(metrics.AttrString("status", status)))
	return questions, nil
}

// buildCatalog renders the document catalog (one DocumentInfo.String() per source,
// newline-joined) the follow-up generator is grounded against. A nil lister yields an
// empty catalog — the hardened prompt then proposes nothing rather than going off-source.
func (p *Pipeline) buildCatalog(ctx context.Context) (string, error) {
	if p.lister == nil {
		return "", nil
	}
	docs, err := p.lister.ListSources(ctx)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(docs))
	for _, d := range docs {
		lines = append(lines, d.String())
	}
	return strings.Join(lines, "\n"), nil
}

// recentUserText concatenates the content of the user-authored turns in history into a
// single string for the Model Armor gate. Assistant turns are ours and skipped.
func recentUserText(history []Message) string {
	parts := make([]string, 0, len(history))
	for _, m := range history {
		if m.Role == RoleUser {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// buildRetrievalStep turns the retrieved chunks into a TraceStep of kind retrieval:
// one GroundingExcerpt per chunk (source + raw content), the chunk count, and the
// count of distinct sources. Excerpts is always non-nil (empty on the zero-chunk path)
// so it serialises to a JSON array rather than null.
func buildRetrievalStep(chunks []RetrievedChunk) TraceStep {
	excerpts := make([]GroundingExcerpt, 0, len(chunks))
	names := make([]string, 0, len(chunks))
	for _, c := range chunks {
		excerpts = append(excerpts, GroundingExcerpt{SourceName: c.SourceName, Text: c.Content})
		names = append(names, c.SourceName)
	}
	return TraceStep{
		Kind:  TraceKindRetrieval,
		Label: "Searched knowledge base",
		Retrieval: &RetrievalDetail{
			ChunkCount:  len(chunks),
			SourceCount: len(DedupeSourceNames(names)),
			Excerpts:    excerpts,
		},
	}
}

// DedupeSourceNames returns the distinct source names from names, preserving
// first-seen order. It is the single citation-attribution primitive shared across
// retrieval paths: Pipeline.Answer (chunk sources then tool-fetched docs), the
// retrieval TraceStep's distinct-source count, and the match_job_description tool
// (evidence sources across requirements) all build their source lists through it,
// so every path attributes identically. Returns a non-nil empty slice for empty input.
func DedupeSourceNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

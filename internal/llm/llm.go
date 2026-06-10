package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"golang.org/x/sync/errgroup"

	"go.opentelemetry.io/otel/metric"

	"github.com/Anthony-Bible/sre-bible/internal/email"
	"github.com/Anthony-Bible/sre-bible/internal/metrics"
	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

const maxToolRounds = 5

// tool_call trace outcomes.
const (
	outcomeOK       = "ok"
	outcomeError    = "error"
	outcomeNotFound = "not_found"
	outcomeRefused  = "refused"
)

// emailTraceLabel is the generic, PII-free label recorded for every send_contact_email
// trace step. The Viewer's name, email, draft, and any refusal reason are NEVER recorded.
const emailTraceLabel = "Drafted a message to Anthony"

// matchTraceLabel is the generic, PII-free label recorded for every match_job_description
// trace step. The Viewer's pasted requirement text is NEVER recorded — only this label,
// an empty target, and the outcome.
const matchTraceLabel = "Matched the job description against Anthony's background"

const (
	toolListDocuments           = "list_documents"
	toolFetchFullDocument       = "fetch_full_document"
	toolSendContactEmail        = "send_contact_email"
	toolMatchJobDescription     = "match_job_description"
	toolEvaluateInterviewAnswer = "evaluate_interview_answer"
	// toolSuggestQuestions is the forced tool SuggestFollowUps uses to coerce the
	// model into returning a JSON {questions:[...]} object instead of prose.
	toolSuggestQuestions = "suggest_questions"
)

// judgeTraceLabel is the generic, PII-free label recorded for every
// evaluate_interview_answer trace step. The Viewer's raw answer text, the
// returned feedback, and the numeric score are NEVER recorded — only this
// label, an empty target, and the outcome.
const judgeTraceLabel = "Graded an interview answer"

// evaluate_interview_answer field names + bounds.
const (
	fieldQuestionIndex   = "question_index"
	fieldQuestionText    = "question_text"
	fieldUserAnswer      = "user_answer"
	maxAnswerChars       = 4000
	maxQuestionTextChars = 1000
	maxJudgeFeedback     = 800
	maxJudgeConcepts     = 8
	maxJudgeConceptChars = 64
)

// fieldQuestions is the schema property + JSON key for the suggest_questions tool input.
const fieldQuestions = "questions"

// typeToolUse is the content-block type the Anthropic SDK uses for a model tool call.
const typeToolUse = "tool_use"

// schema map key constants (used in buildToolParams property maps).
const (
	schemaType        = "type"
	schemaDescription = "description"
	schemaItems       = "items"
	schemaTypeString  = "string"
	schemaTypeArray   = "array"
)

// match_job_description field name + bounds.
const (
	fieldRequirements   = "requirements"
	maxRequirements     = 12  // token/DoS bound on requirements processed per call
	maxRequirementChars = 200 // per-requirement input cap
	maxEvidenceExcerpt  = 300 // per-evidence excerpt cap in the tool result
	matchEvidenceK      = 4   // chunks retrieved per requirement
	matchConcurrency    = 4   // concurrent per-requirement retrievals
)

// slowMatchThreshold is how long match_job_description may run before a one-shot
// notice trace step is emitted so the UI can show "still searching…". Sized to
// cover one rate-limit backoff (≈1s) plus normal embed latency without nagging
// the Viewer on fast runs.
const slowMatchThreshold = 3 * time.Second

// slowMatchNotice is the generic, PII-free label surfaced to the Viewer when a
// match call crosses slowMatchThreshold (typically because the embedding API
// rate-limited us and gemini.retryEmbed is backing off).
const slowMatchNotice = "This is taking a bit longer than usual — still searching…"

// requirementsTruncatedNotice warns the Viewer when more than maxRequirements
// were supplied and the tail was discarded before matching.
const requirementsTruncatedNotice = "Only the first 12 requirements were matched — the rest were dropped."

// send_contact_email field name constants.
const (
	fieldSenderName     = "sender_name"
	fieldSenderEmail    = "sender_email"
	fieldMessage        = "message"
	fieldConfirmedDraft = "confirmed_draft"
)

// Client wraps the Anthropic SDK and satisfies rag.Generator.
type Client struct {
	inner            *anthropic.Client
	model            string
	baseSystemPrompt string
	personas         map[rag.PersonaMode]string
	log              *slog.Logger
	// temperature, when non-nil, pins the sampling temperature. Nil omits the
	// field so the Anthropic API default applies (production's natural variation).
	temperature *float64
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithTemperature pins the sampling temperature for generation. Without it the
// client omits the field entirely and the Anthropic API default applies. The
// eval harness sets temperature=0 so its quality gate measures the agent's
// behaviour rather than sampling noise; production leaves it unset to keep the
// agent's natural conversational variation.
func WithTemperature(t float64) Option {
	return func(c *Client) { c.temperature = &t }
}

// NewClient creates an Anthropic Claude streaming client.
// baseSystemPrompt and personas are used to construct the system prompt dynamically on each call.
func NewClient(apiKey, model, baseSystemPrompt string, personas map[rag.PersonaMode]string, log *slog.Logger, opts ...Option) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	client := &Client{
		inner:            &c,
		model:            model,
		baseSystemPrompt: baseSystemPrompt,
		personas:         personas,
		log:              log,
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// StreamAnswer implements rag.Generator. Sends systemPrompt + messages to Claude,
// invoking onToken for each text delta. Runs a tool-use loop (up to maxToolRounds)
// when the model calls list_documents or fetch_full_document. Aborts if onToken
// returns an error. Returns the names of any documents fetched via fetch_full_document
// so callers can include them in citations.
//
// onTrace, if non-nil, receives one tool_call TraceStep per tool round and a terminal
// answer TraceStep (carrying the tool-round count and wall-clock duration) before each
// non-error return. The pipeline emits the retrieval step that precedes these.
func (c *Client) StreamAnswer(ctx context.Context, messages []rag.Message, tools rag.ToolSet, onToken func(string) error, onTrace func(rag.TraceStep) error) ([]string, error) {
	start := time.Now()
	params := toMessageParams(messages, 2*maxToolRounds)

	toolParams := buildToolParams(tools)
	var fetchedNames []string

	for round := 0; round <= maxToolRounds; round++ {
		reqParams := c.buildRequestParams(ctx, params, toolParams, round)

		acc, err := c.streamOnce(ctx, reqParams, onToken)
		if err != nil {
			return fetchedNames, err
		}

		params = append(params, acc.ToParam())

		if acc.StopReason != anthropic.StopReasonToolUse {
			c.log.InfoContext(ctx, "stream complete", "model", c.model, "rounds", round+1)
			// round = number of tool rounds that ran before this final answer.
			emitAnswerStep(onTrace, round, start)
			return fetchedNames, nil
		}

		toolResults, roundFetched := c.collectToolResults(ctx, round, acc, tools, onTrace)
		fetchedNames = append(fetchedNames, roundFetched...)
		if len(toolResults) == 0 {
			// stop_reason=tool_use but no tool_use blocks — protocol violation; abort.
			return fetchedNames, fmt.Errorf("stream answer: stop_reason=tool_use but response contains no tool_use blocks")
		}
		params = append(params, anthropic.NewUserMessage(toolResults...))
	}

	c.log.InfoContext(ctx, "stream complete (tool cap hit)", "model", c.model)
	emitAnswerStep(onTrace, maxToolRounds, start)
	return fetchedNames, nil
}

// SuggestFollowUps does a single non-streaming generation with a forced
// suggest_questions tool, so the result is guaranteed-valid JSON rather than prose to
// parse. It runs no agentic loop and advertises no RAG tools. systemPrompt is the
// scope-locked rag.FollowUpSystemPrompt; messages is the recent history plus a final
// user turn carrying the document catalog. It returns up to maxQuestions trimmed,
// non-empty questions. Any API, protocol, or JSON error is returned so the caller can
// degrade to "no cards". Implements rag.FollowUpSuggester.
func (c *Client) SuggestFollowUps(ctx context.Context, systemPrompt string, messages []rag.Message, maxQuestions int) ([]string, error) {
	params := toMessageParams(messages, 0)

	tool := anthropic.ToolParam{
		Name:        toolSuggestQuestions,
		Description: anthropic.String("Return the proposed follow-up questions as a JSON array of short strings."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				fieldQuestions: map[string]any{
					schemaType:        schemaTypeArray,
					schemaDescription: "The proposed follow-up questions, each a short string phrased in the visitor's voice.",
					schemaItems:       map[string]any{schemaType: schemaTypeString},
				},
			},
			Required: []string{fieldQuestions},
		},
	}

	reqParams := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: rag.MaxFollowUpTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  params,
		Tools:     []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: toolSuggestQuestions},
		},
	}
	if c.temperature != nil {
		reqParams.Temperature = anthropic.Float(*c.temperature)
	}

	msg, err := c.inner.Messages.New(ctx, reqParams)
	if err != nil {
		return nil, fmt.Errorf("suggest follow-ups: %w", err)
	}
	return parseSuggestedQuestions(msg, maxQuestions)
}

// parseSuggestedQuestions extracts the questions from the forced suggest_questions
// tool_use block in msg, trimming blanks and capping to maxQuestions. It errors when no
// tool_use block is present or its input is not the expected {questions:[...]} shape.
func parseSuggestedQuestions(msg *anthropic.Message, maxQuestions int) ([]string, error) {
	for _, cb := range msg.Content {
		if cb.Type != typeToolUse {
			continue
		}
		tu := cb.AsToolUse()
		var out struct {
			Questions []string `json:"questions"`
		}
		if err := json.Unmarshal(tu.Input, &out); err != nil {
			return nil, fmt.Errorf("suggest follow-ups: decode tool input: %w", err)
		}
		return rag.CapQuestions(out.Questions, maxQuestions), nil
	}
	return nil, fmt.Errorf("suggest follow-ups: no %s tool_use block in response", toolSuggestQuestions)
}

// toMessageParams maps rag.Messages to Anthropic MessageParams as user/assistant text
// blocks. extraCap reserves slack for params the caller appends after the history (e.g.
// per-round tool-result turns in StreamAnswer); pass 0 when nothing is appended.
func toMessageParams(messages []rag.Message, extraCap int) []anthropic.MessageParam {
	params := make([]anthropic.MessageParam, 0, len(messages)+extraCap)
	for _, m := range messages {
		switch m.Role {
		case rag.RoleUser:
			params = append(params, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case rag.RoleAssistant:
			params = append(params, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	return params
}

// buildRequestParams assembles the per-round Anthropic request: the system
// blocks (base prompt plus the active persona for this context), the message
// history, the tool set, and — on the final round — a forced no-tool choice so
// the model produces a terminal answer rather than another tool call. When the
// client has a pinned temperature (eval), it is set; otherwise the field is
// omitted so the API default applies.
func (c *Client) buildRequestParams(ctx context.Context, params []anthropic.MessageParam, toolParams []anthropic.ToolUnionParam, round int) anthropic.MessageNewParams {
	mode := rag.PersonaModeFromContext(ctx)
	var personaText string
	if c.personas != nil {
		personaText = c.personas[mode]
		if personaText == "" {
			personaText = c.personas[rag.ModeStandard]
		}
	}

	systemBlocks := []anthropic.TextBlockParam{
		{Text: c.baseSystemPrompt},
	}
	if personaText != "" {
		systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: personaText})
	}

	reqParams := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: 2048,
		System:    systemBlocks,
		Messages:  params,
	}
	if c.temperature != nil {
		reqParams.Temperature = anthropic.Float(*c.temperature)
	}
	if len(toolParams) > 0 {
		reqParams.Tools = toolParams
		if round >= maxToolRounds {
			none := anthropic.NewToolChoiceNoneParam()
			reqParams.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &none}
		}
	}
	return reqParams
}

// emitTrace delivers one TraceStep to onTrace on a best-effort basis. A nil onTrace is a
// no-op, and a callback error is intentionally swallowed: the step is observability only,
// the caller accumulates the trace independently, and a failed transient write (e.g. a
// disconnected client) must never abort generation.
func emitTrace(onTrace func(rag.TraceStep) error, step rag.TraceStep) {
	if onTrace == nil {
		return
	}
	_ = onTrace(step)
}

// emitAnswerStep emits the terminal answer TraceStep. toolRounds is the number of tool
// rounds that ran; start anchors the wall-clock duration.
func emitAnswerStep(onTrace func(rag.TraceStep) error, toolRounds int, start time.Time) {
	emitTrace(onTrace, rag.TraceStep{
		Kind:  rag.TraceKindAnswer,
		Label: "Composed answer",
		Answer: &rag.AnswerDetail{
			ToolRounds: toolRounds,
			DurationMs: time.Since(start).Milliseconds(),
		},
	})
}

// streamOnce runs a single streaming API call and returns the accumulated message.
func (c *Client) streamOnce(ctx context.Context, reqParams anthropic.MessageNewParams, onToken func(string) error) (anthropic.Message, error) {
	stream := c.inner.Messages.NewStreaming(ctx, reqParams)
	var acc anthropic.Message
	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			return acc, fmt.Errorf("accumulate stream event: %w", err)
		}
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text := delta.Delta.AsTextDelta(); text.Text != "" {
				if err := onToken(text.Text); err != nil {
					return acc, err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return acc, fmt.Errorf("stream answer: %w", err)
	}
	return acc, nil
}

// collectToolResults executes every tool_use block in acc.
// Returns the tool result blocks to send back to the model and the names of any
// documents successfully fetched via fetch_full_document (for citation tracking).
// Each executed tool emits one tool_call TraceStep via onTrace (inside runTool).
func (c *Client) collectToolResults(ctx context.Context, round int, acc anthropic.Message, tools rag.ToolSet, onTrace func(rag.TraceStep) error) ([]anthropic.ContentBlockParamUnion, []string) {
	var results []anthropic.ContentBlockParamUnion
	var fetchedNames []string
	for _, cb := range acc.Content {
		if cb.Type != typeToolUse {
			continue
		}
		tu := cb.AsToolUse()
		c.log.InfoContext(ctx, "tool use", "tool", tu.Name, "round", round+1)
		text, isErr, sources := c.runTool(ctx, tu, tools, onTrace)
		results = append(results, anthropic.NewToolResultBlock(tu.ID, text, isErr))
		fetchedNames = append(fetchedNames, sources...)
	}
	return results, fetchedNames
}

// emitToolCall emits a tool_call TraceStep and records the per-tool metric.
//
// PII rule: callers pass curated, PII-free labels and SAFE targets (document names only).
// For send_contact_email, use emitEmailToolCall instead — it bakes in the PII-free label
// and empty target so the Viewer's email, draft, and reason can never reach a trace.
func emitToolCall(ctx context.Context, onTrace func(rag.TraceStep) error, label, tool, target, outcome string) {
	metrics.M.LLMToolCalls.Add(ctx, 1, metric.WithAttributes(
		metrics.AttrString("tool", tool),
		metrics.AttrString("outcome", outcome),
	))
	emitTrace(onTrace, rag.TraceStep{
		Kind:  rag.TraceKindToolCall,
		Label: label,
		ToolCall: &rag.ToolCallDetail{
			Tool:    tool,
			Target:  target,
			Outcome: outcome,
		},
	})
}

// emitEmailToolCall emits the tool_call TraceStep for send_contact_email with only the
// outcome varying. The PII-free label and empty target are hardcoded here so the email
// path cannot leak the Viewer's name, address, message body, or any refusal reason into a
// trace even by mistake — the structural enforcement of the PII rule, not just convention.
func emitEmailToolCall(ctx context.Context, onTrace func(rag.TraceStep) error, outcome string) {
	emitToolCall(ctx, onTrace, emailTraceLabel, toolSendContactEmail, "", outcome)
}

// buildToolParams returns tool definitions for whichever ToolSet fields are non-nil.
func buildToolParams(tools rag.ToolSet) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, 4)

	if tools.Lister != nil {
		t := anthropic.ToolParam{
			Name:        toolListDocuments,
			Description: anthropic.String("List all available documents in the knowledge base. Returns document names, types, and a short description of each document's contents. Call this before fetch_full_document to discover valid names and choose the right document."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	if tools.Fetcher != nil {
		t := anthropic.ToolParam{
			Name:        toolFetchFullDocument,
			Description: anthropic.String("Fetch the complete text of a document from the knowledge base. Use this when retrieved chunks are insufficient to answer the question completely."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"source_name": map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The exact document name as returned by list_documents.",
					},
				},
				Required: []string{"source_name"},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	if tools.Matcher != nil {
		t := anthropic.ToolParam{
			Name:        toolMatchJobDescription,
			Description: anthropic.String("Map a job description to Anthony's documented background. First extract the distinct requirements from the job description yourself, then pass them as the 'requirements' string array (call this at most once per turn); for each requirement the tool retrieves the most relevant evidence from Anthony's ingested documents and returns it alongside instructions for rendering a Fit Scorecard."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					fieldRequirements: map[string]any{
						schemaType:        schemaTypeArray,
						schemaDescription: "The distinct requirements extracted from the job description, each a short phrase (e.g. \"5+ years operating Kubernetes in production\").",
						schemaItems:       map[string]any{schemaType: schemaTypeString},
					},
				},
				Required: []string{fieldRequirements},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	if tools.Judge != nil {
		t := anthropic.ToolParam{
			Name:        toolEvaluateInterviewAnswer,
			Description: anthropic.String("Grade the candidate's most recent interview answer for the current scenario. Call this exactly once per candidate response, immediately after they answer. Returns {score 0-100, feedback, passed, concepts_demonstrated}. Use the returned score and feedback verbatim in your reply; do not invent your own grade."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					fieldQuestionIndex: map[string]any{
						schemaType:        "integer",
						schemaDescription: "0-based index of the scenario being graded. 0=Cascading failure / cache stampede; 1=BGP route leak / DNS hijack; 2=Serverless cold start / DB connection pool exhaustion.",
					},
					fieldQuestionText: map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The exact scenario question the candidate is answering.",
					},
					fieldUserAnswer: map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The candidate's unstructured response, verbatim.",
					},
				},
				Required: []string{fieldQuestionIndex, fieldQuestionText, fieldUserAnswer},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	if tools.Emailer != nil {
		t := anthropic.ToolParam{
			Name:        toolSendContactEmail,
			Description: anthropic.String("Send an email to Anthony on the visitor's behalf. Only call after: (1) the visitor has explicitly provided their name, email address, and message — never invent or guess these values; and (2) you have shown the visitor a draft of the email and they have confirmed they want to send it. Set confirmed_draft=true only after the visitor has seen and approved the draft."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					fieldSenderName: map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The visitor's full name.",
					},
					fieldSenderEmail: map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The visitor's email address (used as Reply-To).",
					},
					fieldMessage: map[string]any{
						schemaType:        schemaTypeString,
						schemaDescription: "The message body to deliver to Anthony.",
					},
					fieldConfirmedDraft: map[string]any{
						schemaType:        "boolean",
						schemaDescription: "Set to true only after you have shown the visitor a draft of the email and they have explicitly confirmed they want to send it.",
					},
				},
				Required: []string{fieldSenderName, fieldSenderEmail, fieldMessage, fieldConfirmedDraft},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	return result
}

// runTool dispatches a tool_use block and returns the result text, whether it is an
// error, and the source names to fold into citations (empty for tools that cite none).
// Each branch emits exactly one tool_call TraceStep via onTrace with the curated label,
// SAFE target, and mapped outcome.
func (c *Client) runTool(ctx context.Context, tu anthropic.ToolUseBlock, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	switch tu.Name {
	case toolListDocuments:
		return c.runListDocuments(ctx, tools, onTrace)
	case toolFetchFullDocument:
		return c.runFetchFullDocument(ctx, tu.Input, tools, onTrace)
	case toolMatchJobDescription:
		return c.runMatchJobDescription(ctx, tu.Input, tools, onTrace)
	case toolEvaluateInterviewAnswer:
		return c.runEvaluateInterviewAnswer(ctx, tu.Input, tools, onTrace)
	case toolSendContactEmail:
		return c.runSendContactEmail(ctx, tu.Input, tools, onTrace)
	default:
		emitToolCall(ctx, onTrace, "Unknown tool", tu.Name, "", outcomeError)
		return fmt.Sprintf("unknown tool: %s", tu.Name), true, nil
	}
}

func (c *Client) runListDocuments(ctx context.Context, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	const label = "Listing available documents…"
	docs, err := tools.Lister.ListSources(ctx)
	if err != nil {
		emitToolCall(ctx, onTrace, label, toolListDocuments, "", outcomeError)
		return fmt.Sprintf("error listing documents: %v", err), true, nil
	}
	emitToolCall(ctx, onTrace, label, toolListDocuments, "", outcomeOK)
	if len(docs) == 0 {
		return "No documents are available in the knowledge base.", false, nil
	}
	var sb strings.Builder
	for _, d := range docs {
		sb.WriteString(d.String())
		sb.WriteByte('\n')
	}
	return sb.String(), false, nil
}

func (c *Client) runFetchFullDocument(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	var input struct {
		SourceName string `json:"source_name"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		emitToolCall(ctx, onTrace, "Reading document…", toolFetchFullDocument, "", outcomeError)
		return fmt.Sprintf("invalid fetch_full_document arguments: %v", err), true, nil
	}
	label := fmt.Sprintf("Reading %s…", input.SourceName)
	text, found, err := tools.Fetcher.GetFullText(ctx, input.SourceName)
	if err != nil {
		emitToolCall(ctx, onTrace, label, toolFetchFullDocument, input.SourceName, outcomeError)
		return fmt.Sprintf("error fetching document: %v", err), true, nil
	}
	if !found {
		emitToolCall(ctx, onTrace, label, toolFetchFullDocument, input.SourceName, outcomeNotFound)
		return fmt.Sprintf("No document named %q is available (or it has no stored full text). Use list_documents to see valid names.", input.SourceName), false, nil
	}
	emitToolCall(ctx, onTrace, label, toolFetchFullDocument, input.SourceName, outcomeOK)
	return text, false, []string{input.SourceName}
}

func (c *Client) runSendContactEmail(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	var input struct {
		SenderName     string `json:"sender_name"`
		SenderEmail    string `json:"sender_email"`
		Message        string `json:"message"`
		ConfirmedDraft bool   `json:"confirmed_draft"`
	}
	// Every trace step below goes through emitEmailToolCall, which bakes in the generic
	// label and empty target — the Viewer's name, email, message body, and any refusal
	// reason are never recorded.
	if err := json.Unmarshal(rawInput, &input); err != nil {
		emitEmailToolCall(ctx, onTrace, outcomeError)
		return fmt.Sprintf("invalid send_contact_email arguments: %v", err), true, nil
	}
	if !input.ConfirmedDraft {
		emitEmailToolCall(ctx, onTrace, outcomeRefused)
		return "You must show the visitor a draft of the email and get their explicit confirmation before sending. Please present the draft and ask the visitor to confirm.", true, nil
	}
	ok, reason, err := tools.Emailer.SendContactEmail(ctx, email.ContactEmail{
		SenderName:  input.SenderName,
		SenderEmail: input.SenderEmail,
		Message:     input.Message,
	})
	if err != nil {
		c.log.ErrorContext(ctx, "send contact email", slog.Any("err", err))
		emitEmailToolCall(ctx, onTrace, outcomeError)
		return "The email could not be sent due to an internal error. Apologize briefly to the visitor and suggest they reach Anthony at linkedin.com/in/anthonybible/ instead.", true, nil
	}
	if !ok {
		emitEmailToolCall(ctx, onTrace, outcomeRefused)
		return reason, true, nil
	}
	emitEmailToolCall(ctx, onTrace, outcomeOK)
	return "Your message was sent to Anthony successfully. Confirm this to the visitor.", false, nil
}

// matchEvidence is one cited excerpt supporting a requirement in the tool result.
//
// It deliberately mirrors rag.GroundingExcerpt in shape (excerpt text + source name)
// but is intentionally NOT unified with it: the two live at different layers with
// different wire formats and consumers. matchEvidence is serialised into the tool
// result fed back to the model (json "excerpt"/"source"); rag.GroundingExcerpt is
// persisted into the Agent Trace JSONB (json "text"/"source_name"). Sharing one struct
// would couple the model-facing tool contract to the trace persistence schema, so the
// duplicated shape is by design, not an oversight.
type matchEvidence struct {
	Excerpt string `json:"excerpt"`
	Source  string `json:"source"`
}

// matchRequirementResult is the per-requirement evidence; empty Evidence is a Gap.
type matchRequirementResult struct {
	Requirement string          `json:"requirement"`
	Evidence    []matchEvidence `json:"evidence"`
}

// matchJobResult is the structured tool result the model turns into a Fit Scorecard.
// Instructions rides in the result (not the static tool schema) so the rendering recipe
// sits next to the evidence it governs and is only spent when the tool actually runs.
type matchJobResult struct {
	Instructions string                   `json:"instructions"`
	Requirements []matchRequirementResult `json:"requirements"`
}

// matchRenderInstructions tells the model how to turn the returned evidence into a Fit
// Scorecard. It is returned in the tool result (the "tool response") rather than baked
// into the tool description.
const matchRenderInstructions = "Render this as a Fit Scorecard: a GitHub-flavored Markdown table with the columns Requirement | Match | Evidence. Classify each requirement from its evidence — Strong (clear, directly cited evidence), Partial (related but incomplete cited evidence), or Gap (no evidence). Cite a specific source for every evidence item; never fabricate evidence and never infer a Match from your own knowledge. Word every Gap neutrally (\"No supporting evidence in Anthony's documented background\"), making no claim either way about whether Anthony has the skill. After the table, give a brief overall fit summary, and when any Gap exists, invite the visitor to ask Anthony directly via the send_contact_email tool."

// runMatchJobDescription retrieves grounded evidence for each pre-extracted
// requirement (decomposition is the model's job, not the tool's) and returns a
// structured JSON result plus the deduped source names for citations. It makes no
// LLM call and never logs or traces requirement/JD text — only counts and duration.
// It emits exactly one tool_call TraceStep with the generic matchTraceLabel, an empty
// target, and the mapped outcome (the Viewer's requirement text never reaches the trace).
func (c *Client) runMatchJobDescription(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	var input struct {
		Requirements []string `json:"requirements"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		emitToolCall(ctx, onTrace, matchTraceLabel, toolMatchJobDescription, "", outcomeError)
		return fmt.Sprintf("invalid match_job_description arguments: %v", err), true, nil
	}

	reqs := make([]string, 0, len(input.Requirements))
	for _, r := range input.Requirements {
		if r = strings.TrimSpace(r); r != "" {
			reqs = append(reqs, truncateText(r, maxRequirementChars))
		}
	}
	if len(reqs) == 0 {
		emitToolCall(ctx, onTrace, matchTraceLabel, toolMatchJobDescription, "", outcomeError)
		return "No requirements provided. Extract the distinct requirements from the job description and pass them as the 'requirements' array.", true, nil
	}
	if len(reqs) > maxRequirements {
		reqs = reqs[:maxRequirements]
		emitTrace(onTrace, rag.TraceStep{Kind: rag.TraceKindNotice, Label: requirementsTruncatedNotice})
	}

	start := time.Now()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTimer(slowMatchThreshold)
		defer t.Stop()
		select {
		case <-done:
		case <-ctx.Done():
		case <-t.C:
			emitTrace(onTrace, rag.TraceStep{Kind: rag.TraceKindNotice, Label: slowMatchNotice})
		}
	}()

	chunksByReq := make([][]rag.RetrievedChunk, len(reqs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(matchConcurrency)
	for i, req := range reqs {
		g.Go(func() error {
			chunks, err := tools.Matcher.MatchRequirement(gctx, req, matchEvidenceK)
			if err != nil {
				return err
			}
			chunksByReq[i] = chunks
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		c.log.ErrorContext(ctx, "match job description", slog.Any("err", err))
		emitToolCall(ctx, onTrace, matchTraceLabel, toolMatchJobDescription, "", outcomeError)
		return "The evidence search failed due to an internal error. Apologize briefly and suggest the visitor try again.", true, nil
	}

	var names []string
	out := matchJobResult{Instructions: matchRenderInstructions, Requirements: make([]matchRequirementResult, len(reqs))}
	for i, req := range reqs {
		evidence := make([]matchEvidence, 0, len(chunksByReq[i]))
		for _, ch := range chunksByReq[i] {
			evidence = append(evidence, matchEvidence{
				Excerpt: truncateText(ch.Content, maxEvidenceExcerpt),
				Source:  ch.SourceName,
			})
			names = append(names, ch.SourceName)
		}
		out.Requirements[i] = matchRequirementResult{Requirement: req, Evidence: evidence}
	}
	// Deduped citation sources across all requirements, in first-seen order — the same
	// attribution primitive the RAG pipeline uses for chunk citations.
	sources := rag.DedupeSourceNames(names)

	payload, err := json.Marshal(out)
	if err != nil {
		emitToolCall(ctx, onTrace, matchTraceLabel, toolMatchJobDescription, "", outcomeError)
		return fmt.Sprintf("error encoding evidence: %v", err), true, nil
	}

	emitToolCall(ctx, onTrace, matchTraceLabel, toolMatchJobDescription, "", outcomeOK)
	c.log.InfoContext(ctx, "match job description",
		"requirements_count", len(reqs),
		"sources_cited", len(sources),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return string(payload), false, sources
}

// runEvaluateInterviewAnswer dispatches one interview answer to the LLM judge,
// returning the InterviewEvaluation as JSON for the parent agent loop. Mirrors the
// match_job_description pattern: validate input, delegate to a typed tools.X
// implementation, return JSON, and emit exactly one tool_call TraceStep with the
// generic judgeTraceLabel — never the user's answer, the judge's feedback, or the
// numeric score (the trace records only the curated label and the outcome).
func (c *Client) runEvaluateInterviewAnswer(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, []string) {
	// judgeFail emits the single tool_call trace step required on every error
	// exit and formats the user-facing error string. Centralizing this means a
	// future change to the trace label or outcome value can't drift across the
	// five validation branches below.
	judgeFail := func(format string, args ...any) (string, bool, []string) {
		emitToolCall(ctx, onTrace, judgeTraceLabel, toolEvaluateInterviewAnswer, "", outcomeError)
		return fmt.Sprintf(format, args...), true, nil
	}

	var input struct {
		QuestionIndex *int   `json:"question_index"`
		QuestionText  string `json:"question_text"`
		UserAnswer    string `json:"user_answer"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return judgeFail("invalid evaluate_interview_answer arguments: %v", err)
	}
	if input.QuestionIndex == nil {
		return judgeFail("missing required field %q; must be an integer in [0, %d).", fieldQuestionIndex, rag.InterviewNumScenarios)
	}
	qIdx := *input.QuestionIndex
	if qIdx < 0 || qIdx >= rag.InterviewNumScenarios {
		return judgeFail("question_index %d is out of range; must be in [0, %d).", qIdx, rag.InterviewNumScenarios)
	}

	answer := strings.TrimSpace(input.UserAnswer)
	if answer == "" {
		return judgeFail("user_answer is empty; ask the candidate to provide their response to the scenario before grading.")
	}
	answer = truncateText(answer, maxAnswerChars)
	qText := truncateText(strings.TrimSpace(input.QuestionText), maxQuestionTextChars)

	start := time.Now()
	eval, err := tools.Judge.EvaluateAnswer(ctx, qIdx, qText, answer)
	if err != nil {
		c.log.ErrorContext(ctx, "evaluate interview answer", slog.Any("err", err))
		return judgeFail("The grader could not score this answer due to an internal error. Apologize briefly and suggest the candidate restate their answer.")
	}
	if eval == nil {
		return judgeFail("The grader returned no result. Apologize briefly and suggest the candidate restate their answer.")
	}

	normalized := normalizeEvaluation(eval)
	payload, err := json.Marshal(normalized)
	if err != nil {
		return judgeFail("error encoding evaluation: %v", err)
	}

	emitToolCall(ctx, onTrace, judgeTraceLabel, toolEvaluateInterviewAnswer, "", outcomeOK)
	c.log.InfoContext(ctx, "evaluate interview answer",
		"question_index", qIdx,
		"answer_chars", len(answer),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return string(payload), false, nil
}

// normalizeEvaluation clamps and trims a Judge result to safe bounds before it is
// marshalled back to the agent loop. Score is clamped to [0,100]; Passed is derived
// from Score (>=60) so it cannot drift from the score the model sees; feedback and
// concept strings are length-capped; concepts beyond maxJudgeConcepts are dropped.
//
// Concepts are filtered for empties BEFORE the maxJudgeConcepts cap so a few
// leading whitespace-only entries can't squeeze valid concepts out of the result.
func normalizeEvaluation(in *rag.InterviewEvaluation) rag.InterviewEvaluation {
	score := in.Score
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	concepts := make([]string, 0, len(in.ConceptsDemonstrated))
	for _, c := range in.ConceptsDemonstrated {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		concepts = append(concepts, truncateText(c, maxJudgeConceptChars))
		if len(concepts) == maxJudgeConcepts {
			break
		}
	}
	return rag.InterviewEvaluation{
		Score:                score,
		Feedback:             truncateText(strings.TrimSpace(in.Feedback), maxJudgeFeedback),
		Passed:               score >= 60,
		ConceptsDemonstrated: concepts,
	}
}

// truncateText caps s to at most maxRunes runes, splitting on a rune boundary.
func truncateText(s string, maxRunes int) string {
	// Byte length bounds rune count, so a short string never needs conversion.
	if len(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

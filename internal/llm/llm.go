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

	"github.com/Anthony-Bible/sre-bible/internal/email"
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

const (
	toolListDocuments     = "list_documents"
	toolFetchFullDocument = "fetch_full_document"
	toolSendContactEmail  = "send_contact_email"
)

// schema map key constants (used in buildToolParams property maps).
const (
	schemaType        = "type"
	schemaDescription = "description"
	schemaTypeString  = "string"
)

// send_contact_email field name constants.
const (
	fieldSenderName     = "sender_name"
	fieldSenderEmail    = "sender_email"
	fieldMessage        = "message"
	fieldConfirmedDraft = "confirmed_draft"
)

// Client wraps the Anthropic SDK and satisfies rag.Generator.
type Client struct {
	inner        *anthropic.Client
	model        string
	systemPrompt string
	log          *slog.Logger
}

// NewClient creates an Anthropic Claude streaming client.
// systemPrompt is sent on every call; model is e.g. "claude-haiku-4-5-20251001".
func NewClient(apiKey, model, systemPrompt string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Client{
		inner:        &c,
		model:        model,
		systemPrompt: systemPrompt,
		log:          log,
	}
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
	params := make([]anthropic.MessageParam, 0, len(messages)+2*maxToolRounds)
	for _, m := range messages {
		switch m.Role {
		case rag.RoleUser:
			params = append(params, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case rag.RoleAssistant:
			params = append(params, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}

	toolParams := buildToolParams(tools)
	var fetchedNames []string

	for round := 0; round <= maxToolRounds; round++ {
		reqParams := anthropic.MessageNewParams{
			Model:     c.model,
			MaxTokens: 2048,
			System:    []anthropic.TextBlockParam{{Text: c.systemPrompt}},
			Messages:  params,
		}
		if len(toolParams) > 0 {
			reqParams.Tools = toolParams
			if round >= maxToolRounds {
				none := anthropic.NewToolChoiceNoneParam()
				reqParams.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &none}
			}
		}

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

// emitAnswerStep emits the terminal answer TraceStep. toolRounds is the number of tool
// rounds that ran; start anchors the wall-clock duration. A nil onTrace is a no-op, and
// a callback error is intentionally swallowed: the answer is already produced, the trace
// is accumulated independently by the caller, and a failed transient write must not abort.
func emitAnswerStep(onTrace func(rag.TraceStep) error, toolRounds int, start time.Time) {
	if onTrace == nil {
		return
	}
	_ = onTrace(rag.TraceStep{
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
		if cb.Type != "tool_use" {
			continue
		}
		tu := cb.AsToolUse()
		c.log.InfoContext(ctx, "tool use", "tool", tu.Name, "round", round+1)
		text, isErr, sourceName := c.runTool(ctx, tu, tools, onTrace)
		results = append(results, anthropic.NewToolResultBlock(tu.ID, text, isErr))
		if sourceName != "" {
			fetchedNames = append(fetchedNames, sourceName)
		}
	}
	return results, fetchedNames
}

// emitToolCall emits a tool_call TraceStep. A nil onTrace is a no-op, and a callback
// error is intentionally swallowed: the tool has already run, the trace is accumulated
// independently by the caller, and a failed transient write must not abort generation.
//
// PII rule: callers pass curated, PII-free labels and SAFE targets (document names only).
// For send_contact_email, target is always "" — the Viewer's email, draft, and reason
// are never passed here.
func emitToolCall(onTrace func(rag.TraceStep) error, label, tool, target, outcome string) {
	if onTrace == nil {
		return
	}
	_ = onTrace(rag.TraceStep{
		Kind:  rag.TraceKindToolCall,
		Label: label,
		ToolCall: &rag.ToolCallDetail{
			Tool:    tool,
			Target:  target,
			Outcome: outcome,
		},
	})
}

// buildToolParams returns tool definitions for whichever ToolSet fields are non-nil.
func buildToolParams(tools rag.ToolSet) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, 2)

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
// error, and (for successful fetch_full_document calls) the fetched source name.
// Each branch emits exactly one tool_call TraceStep via onTrace with the curated label,
// SAFE target, and mapped outcome.
func (c *Client) runTool(ctx context.Context, tu anthropic.ToolUseBlock, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, string) {
	switch tu.Name {
	case toolListDocuments:
		return c.runListDocuments(ctx, tools, onTrace)
	case toolFetchFullDocument:
		return c.runFetchFullDocument(ctx, tu.Input, tools, onTrace)
	case toolSendContactEmail:
		return c.runSendContactEmail(ctx, tu.Input, tools, onTrace)
	default:
		emitToolCall(onTrace, "Unknown tool", tu.Name, "", outcomeError)
		return fmt.Sprintf("unknown tool: %s", tu.Name), true, ""
	}
}

func (c *Client) runListDocuments(ctx context.Context, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, string) {
	const label = "Listing available documents…"
	docs, err := tools.Lister.ListSources(ctx)
	if err != nil {
		emitToolCall(onTrace, label, toolListDocuments, "", outcomeError)
		return fmt.Sprintf("error listing documents: %v", err), true, ""
	}
	emitToolCall(onTrace, label, toolListDocuments, "", outcomeOK)
	if len(docs) == 0 {
		return "No documents are available in the knowledge base.", false, ""
	}
	var sb strings.Builder
	for _, d := range docs {
		sb.WriteString(d.String())
		sb.WriteByte('\n')
	}
	return sb.String(), false, ""
}

func (c *Client) runFetchFullDocument(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, string) {
	var input struct {
		SourceName string `json:"source_name"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		emitToolCall(onTrace, "Reading document…", toolFetchFullDocument, "", outcomeError)
		return fmt.Sprintf("invalid fetch_full_document arguments: %v", err), true, ""
	}
	label := fmt.Sprintf("Reading %s…", input.SourceName)
	text, found, err := tools.Fetcher.GetFullText(ctx, input.SourceName)
	if err != nil {
		emitToolCall(onTrace, label, toolFetchFullDocument, input.SourceName, outcomeError)
		return fmt.Sprintf("error fetching document: %v", err), true, ""
	}
	if !found {
		emitToolCall(onTrace, label, toolFetchFullDocument, input.SourceName, outcomeNotFound)
		return fmt.Sprintf("No document named %q is available (or it has no stored full text). Use list_documents to see valid names.", input.SourceName), false, ""
	}
	emitToolCall(onTrace, label, toolFetchFullDocument, input.SourceName, outcomeOK)
	return text, false, input.SourceName
}

func (c *Client) runSendContactEmail(ctx context.Context, rawInput json.RawMessage, tools rag.ToolSet, onTrace func(rag.TraceStep) error) (string, bool, string) {
	var input struct {
		SenderName     string `json:"sender_name"`
		SenderEmail    string `json:"sender_email"`
		Message        string `json:"message"`
		ConfirmedDraft bool   `json:"confirmed_draft"`
	}
	// Every trace step below uses the generic emailTraceLabel and an empty target —
	// the Viewer's name, email, message body, and any refusal reason are never recorded.
	if err := json.Unmarshal(rawInput, &input); err != nil {
		emitToolCall(onTrace, emailTraceLabel, toolSendContactEmail, "", outcomeError)
		return fmt.Sprintf("invalid send_contact_email arguments: %v", err), true, ""
	}
	if !input.ConfirmedDraft {
		emitToolCall(onTrace, emailTraceLabel, toolSendContactEmail, "", outcomeRefused)
		return "You must show the visitor a draft of the email and get their explicit confirmation before sending. Please present the draft and ask the visitor to confirm.", true, ""
	}
	ok, reason, err := tools.Emailer.SendContactEmail(ctx, email.ContactEmail{
		SenderName:  input.SenderName,
		SenderEmail: input.SenderEmail,
		Message:     input.Message,
	})
	if err != nil {
		c.log.ErrorContext(ctx, "send contact email", slog.Any("err", err))
		emitToolCall(onTrace, emailTraceLabel, toolSendContactEmail, "", outcomeError)
		return "The email could not be sent due to an internal error. Apologize briefly to the visitor and suggest they reach Anthony at linkedin.com/in/anthonybible/ instead.", true, ""
	}
	if !ok {
		emitToolCall(onTrace, emailTraceLabel, toolSendContactEmail, "", outcomeRefused)
		return reason, true, ""
	}
	emitToolCall(onTrace, emailTraceLabel, toolSendContactEmail, "", outcomeOK)
	return "Your message was sent to Anthony successfully. Confirm this to the visitor.", false, ""
}

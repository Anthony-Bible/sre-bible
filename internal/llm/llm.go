package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

const maxToolRounds = 5

const (
	toolListDocuments     = "list_documents"
	toolFetchFullDocument = "fetch_full_document"
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
func (c *Client) StreamAnswer(ctx context.Context, messages []rag.Message, tools rag.ToolSet, onToken func(string) error, onStatus func(string) error) ([]string, error) {
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
			return fetchedNames, nil
		}

		toolResults, roundFetched := c.collectToolResults(ctx, round, acc, tools, onStatus)
		fetchedNames = append(fetchedNames, roundFetched...)
		if len(toolResults) == 0 {
			// stop_reason=tool_use but no tool_use blocks — protocol violation; abort.
			return fetchedNames, fmt.Errorf("stream answer: stop_reason=tool_use but response contains no tool_use blocks")
		}
		params = append(params, anthropic.NewUserMessage(toolResults...))
	}

	c.log.InfoContext(ctx, "stream complete (tool cap hit)", "model", c.model)
	return fetchedNames, nil
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
func (c *Client) collectToolResults(ctx context.Context, round int, acc anthropic.Message, tools rag.ToolSet, onStatus func(string) error) ([]anthropic.ContentBlockParamUnion, []string) {
	var results []anthropic.ContentBlockParamUnion
	var fetchedNames []string
	for _, cb := range acc.Content {
		if cb.Type != "tool_use" {
			continue
		}
		tu := cb.AsToolUse()
		c.log.InfoContext(ctx, "tool use", "tool", tu.Name, "round", round+1)
		text, isErr, sourceName := c.runTool(ctx, tu, tools, onStatus)
		results = append(results, anthropic.NewToolResultBlock(tu.ID, text, isErr))
		if sourceName != "" {
			fetchedNames = append(fetchedNames, sourceName)
		}
	}
	return results, fetchedNames
}

// buildToolParams returns tool definitions for whichever ToolSet fields are non-nil.
func buildToolParams(tools rag.ToolSet) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, 2)

	if tools.Lister != nil {
		t := anthropic.ToolParam{
			Name:        toolListDocuments,
			Description: anthropic.String("List all available documents in the knowledge base. Returns document names and types. Call this before fetch_full_document to discover valid names."),
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
						"type":        "string",
						"description": "The exact document name as returned by list_documents.",
					},
				},
				Required: []string{"source_name"},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	return result
}

// runTool dispatches a tool_use block and returns the result text, whether it is an
// error, and (for successful fetch_full_document calls) the fetched source name.
func (c *Client) runTool(ctx context.Context, tu anthropic.ToolUseBlock, tools rag.ToolSet, onStatus func(string) error) (string, bool, string) {
	if onStatus == nil {
		onStatus = func(string) error { return nil }
	}
	switch tu.Name {
	case toolListDocuments:
		_ = onStatus("Listing available documents…")
		docs, err := tools.Lister.ListSources(ctx)
		if err != nil {
			return fmt.Sprintf("error listing documents: %v", err), true, ""
		}
		var sb strings.Builder
		for _, d := range docs {
			fmt.Fprintf(&sb, "%s (%s)\n", d.Name, d.Type)
		}
		return sb.String(), false, ""

	case toolFetchFullDocument:
		var input struct {
			SourceName string `json:"source_name"`
		}
		if err := json.Unmarshal(tu.Input, &input); err != nil {
			return fmt.Sprintf("invalid fetch_full_document arguments: %v", err), true, ""
		}
		_ = onStatus(fmt.Sprintf("Reading %s…", input.SourceName))
		text, found, err := tools.Fetcher.GetFullText(ctx, input.SourceName)
		if err != nil {
			return fmt.Sprintf("error fetching document: %v", err), true, ""
		}
		if !found {
			return fmt.Sprintf("No document named %q is available (or it has no stored full text). Use list_documents to see valid names.", input.SourceName), false, ""
		}
		return text, false, input.SourceName

	default:
		return fmt.Sprintf("unknown tool: %s", tu.Name), true, ""
	}
}

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
// returns an error.
func (c *Client) StreamAnswer(ctx context.Context, messages []rag.Message, tools rag.ToolSet, onToken func(string) error, onStatus func(string) error) error {
	params := make([]anthropic.MessageParam, len(messages))
	for i, m := range messages {
		switch m.Role {
		case rag.RoleUser:
			params[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
		case rag.RoleAssistant:
			params[i] = anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content))
		}
	}

	toolParams := buildToolParams(tools)

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

		stream := c.inner.Messages.NewStreaming(ctx, reqParams)

		var acc anthropic.Message
		for stream.Next() {
			event := stream.Current()
			if err := acc.Accumulate(event); err != nil {
				return fmt.Errorf("accumulate stream event: %w", err)
			}
			if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if text := delta.Delta.AsTextDelta(); text.Text != "" {
					if err := onToken(text.Text); err != nil {
						return err
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("stream answer: %w", err)
		}

		params = append(params, acc.ToParam())

		if acc.StopReason != anthropic.StopReasonToolUse {
			c.log.InfoContext(ctx, "stream complete", "model", c.model, "rounds", round+1)
			return nil
		}

		// Execute every tool_use block; the API requires a result for each.
		var toolResults []anthropic.ContentBlockParamUnion
		for _, cb := range acc.Content {
			if cb.Type != "tool_use" {
				continue
			}
			tu := cb.AsToolUse()
			c.log.InfoContext(ctx, "tool use", "tool", tu.Name, "round", round+1)
			text, isErr := c.runTool(ctx, tu, tools, onStatus)
			toolResults = append(toolResults, anthropic.NewToolResultBlock(tu.ID, text, isErr))
		}
		if len(toolResults) > 0 {
			params = append(params, anthropic.NewUserMessage(toolResults...))
		}
	}

	c.log.InfoContext(ctx, "stream complete (tool cap hit)", "model", c.model)
	return nil
}

// buildToolParams returns tool definitions for whichever ToolSet fields are non-nil.
func buildToolParams(tools rag.ToolSet) []anthropic.ToolUnionParam {
	var result []anthropic.ToolUnionParam

	if tools.Lister != nil {
		t := anthropic.ToolParam{
			Name:        "list_documents",
			Description: anthropic.String("List all available documents in the knowledge base. Returns document names and types. Call this before fetch_full_document to discover valid names."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{},
			},
		}
		result = append(result, anthropic.ToolUnionParam{OfTool: &t})
	}

	if tools.Fetcher != nil {
		t := anthropic.ToolParam{
			Name:        "fetch_full_document",
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

// runTool dispatches a tool_use block and returns the result text and whether it is an error.
func (c *Client) runTool(ctx context.Context, tu anthropic.ToolUseBlock, tools rag.ToolSet, onStatus func(string) error) (string, bool) {
	switch tu.Name {
	case "list_documents":
		if onStatus != nil {
			_ = onStatus("Listing available documents…")
		}
		docs, err := tools.Lister.ListSources(ctx)
		if err != nil {
			return fmt.Sprintf("error listing documents: %v", err), true
		}
		var sb strings.Builder
		for _, d := range docs {
			fmt.Fprintf(&sb, "%s (%s)\n", d.Name, d.Type)
		}
		return sb.String(), false

	case "fetch_full_document":
		var input struct {
			SourceName string `json:"source_name"`
		}
		if err := json.Unmarshal(tu.Input, &input); err != nil {
			return fmt.Sprintf("invalid fetch_full_document arguments: %v", err), true
		}
		if onStatus != nil {
			_ = onStatus(fmt.Sprintf("Reading %s…", input.SourceName))
		}
		text, found, err := tools.Fetcher.GetFullText(ctx, input.SourceName)
		if err != nil {
			return fmt.Sprintf("error fetching document: %v", err), true
		}
		if !found {
			return fmt.Sprintf("No document named %q is available (or it has no stored full text). Use list_documents to see valid names.", input.SourceName), false
		}
		return text, false

	default:
		return fmt.Sprintf("unknown tool: %s", tu.Name), true
	}
}

package llm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
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
// invoking onToken for each text delta. Aborts if onToken returns an error.
func (c *Client) StreamAnswer(ctx context.Context, messages []rag.Message, onToken func(string) error) error {
	params := make([]anthropic.MessageParam, len(messages))
	for i, m := range messages {
		switch m.Role {
		case rag.RoleUser:
			params[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
		case rag.RoleAssistant:
			params[i] = anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content))
		}
	}

	stream := c.inner.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: 2048,
		System:    []anthropic.TextBlockParam{{Text: c.systemPrompt}},
		Messages:  params,
	})

	for stream.Next() {
		event := stream.Current()
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

	c.log.InfoContext(ctx, "stream complete", "model", c.model)
	return nil
}

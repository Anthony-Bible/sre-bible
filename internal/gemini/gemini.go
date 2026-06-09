package gemini

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/genai"
)

// Client wraps the Google GenAI SDK client.
type Client struct {
	inner *genai.Client
	log   *slog.Logger
}

// NewClient creates a Gemini API client authenticated with apiKey.
func NewClient(ctx context.Context, apiKey string, log *slog.Logger) (*Client, error) {
	c, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("create genai client: %w", err)
	}
	return &Client{inner: c, log: log}, nil
}

// generateContent runs a single-turn generation with the given model and parts,
// returning the response text. Callers wrap errors with additional context.
func (c *Client) generateContent(ctx context.Context, model string, parts []*genai.Part) (string, error) {
	resp, err := c.inner.Models.GenerateContent(ctx, model,
		[]*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)},
		nil)
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

// GenerateText runs a single-turn, deterministic (temperature=0) generation with
// the given model and prompt, returning the response text. It is intended for
// evaluation and classification use where reproducibility matters. maxOutputTokens
// bounds the response; pass a value comfortably larger than the expected output so
// a "thinking" model has headroom to reason and still emit its final answer.
// Callers wrap errors with additional context.
func (c *Client) GenerateText(ctx context.Context, model, prompt string, maxOutputTokens int32) (string, error) {
	resp, err := c.inner.Models.GenerateContent(ctx, model,
		[]*genai.Content{genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(prompt)}, genai.RoleUser)},
		&genai.GenerateContentConfig{
			Temperature:     genai.Ptr[float32](0),
			MaxOutputTokens: maxOutputTokens,
		})
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

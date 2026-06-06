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

package gemini

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

const (
	descriptionModel         = "gemini-3.1-flash-lite"
	descriptionMaxInputRunes = 12000
)

const descriptionPrompt = "Write a 1-2 sentence description (max ~40 words) of the " +
	"following document for a knowledge-base index. State what the document is and the key " +
	"topics it covers, so an assistant can decide whether to retrieve it. " +
	"Output only the description, with no preamble or quotes.\n\n"

// Describe generates a short natural-language summary of the provided text.
// The input is truncated to descriptionMaxInputRunes runes to bound cost and latency.
func (c *Client) Describe(ctx context.Context, text string) (string, error) {
	// Outer byte-length check avoids the []rune allocation for short inputs
	// (byte count >= rune count, so if bytes fit, runes definitely fit).
	if len(text) > descriptionMaxInputRunes {
		if runes := []rune(text); len(runes) > descriptionMaxInputRunes {
			text = string(runes[:descriptionMaxInputRunes])
		}
	}
	result, err := c.generateContent(ctx, descriptionModel, []*genai.Part{
		genai.NewPartFromText(descriptionPrompt + text),
	})
	if err != nil {
		return "", fmt.Errorf("generate description: %w", err)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return "", fmt.Errorf("model returned empty description (content may have been filtered)")
	}
	return result, nil
}

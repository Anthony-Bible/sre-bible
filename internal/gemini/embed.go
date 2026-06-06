package gemini

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

const (
	embeddingModel = "gemini-embedding-2"
	embeddingDims  = int32(768)
)

// EmbedDocuments returns a 768-dim embedding for each text using gemini-embedding-2.
// The texts slice is sent as a single batch request.
func (c *Client) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	contents := make([]*genai.Content, len(texts))
	for i, t := range texts {
		contents[i] = &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{genai.NewPartFromText(t)},
		}
	}

	dims := embeddingDims
	resp, err := c.inner.Models.EmbedContent(ctx, embeddingModel, contents,
		&genai.EmbedContentConfig{
			TaskType:             "RETRIEVAL_DOCUMENT",
			OutputDimensionality: &dims,
		})
	if err != nil {
		return nil, fmt.Errorf("embed content: %w", err)
	}

	result := make([][]float32, len(resp.Embeddings))
	for i, e := range resp.Embeddings {
		result[i] = e.Values
	}

	c.log.InfoContext(ctx, "embedded batch", "count", len(texts))
	return result, nil
}

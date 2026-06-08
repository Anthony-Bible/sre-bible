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
	resp, err := retryEmbed(ctx, c.log, "embed_documents", func(ctx context.Context) (*genai.EmbedContentResponse, error) {
		return c.inner.Models.EmbedContent(ctx, embeddingModel, contents,
			&genai.EmbedContentConfig{
				TaskType:             "RETRIEVAL_DOCUMENT",
				OutputDimensionality: &dims,
			})
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

// EmbedQuery returns a 768-dim embedding for a single query text.
// Uses RETRIEVAL_QUERY task type — distinct from RETRIEVAL_DOCUMENT used at ingest time.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	content := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText(text)},
	}

	dims := embeddingDims
	resp, err := retryEmbed(ctx, c.log, "embed_query", func(ctx context.Context) (*genai.EmbedContentResponse, error) {
		return c.inner.Models.EmbedContent(ctx, embeddingModel, []*genai.Content{content},
			&genai.EmbedContentConfig{
				TaskType:             "RETRIEVAL_QUERY",
				OutputDimensionality: &dims,
			})
	})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("embed query: no embeddings returned")
	}

	c.log.InfoContext(ctx, "embedded query")
	return resp.Embeddings[0].Values, nil
}

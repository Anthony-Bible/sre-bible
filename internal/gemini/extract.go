package gemini

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

const extractionModel = "gemini-2.0-flash"

const extractionPrompt = "Extract the full text content of this document as clean markdown. " +
	"Preserve headings, bullet points, and structure. Do not add commentary or explanation."

// ExtractPDFText sends a PDF file to Gemini for document understanding and
// returns the extracted text as clean markdown.
func (c *Client) ExtractPDFText(ctx context.Context, pdfPath string) (string, error) {
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}

	resp, err := c.inner.Models.GenerateContent(ctx, extractionModel,
		[]*genai.Content{
			genai.NewContentFromParts([]*genai.Part{
				genai.NewPartFromBytes(data, "application/pdf"),
				genai.NewPartFromText(extractionPrompt),
			}, genai.RoleUser),
		}, nil)
	if err != nil {
		return "", fmt.Errorf("generate content: %w", err)
	}

	return resp.Text(), nil
}

package gemini

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

const piiScreenModel = "gemini-3.1-flash-lite"

const piiScreenPrompt = "Return the following text verbatim, making only these replacements:\n" +
	"- Replace phone numbers (any format) with [redacted]\n" +
	"- Replace home or street addresses with [redacted]\n" +
	"- Replace ALL email addresses with [redacted] — including professional ones\n" +
	"- Replace government-issued ID numbers (SSN, passport number, national ID) with [redacted]\n" +
	"- Replace dates of birth with [redacted]\n" +
	"\n" +
	"Do NOT replace or modify:\n" +
	"- LinkedIn URLs (e.g. linkedin.com/in/...)\n" +
	"- GitHub URLs (e.g. github.com/...)\n" +
	"- Any other text — reproduce it exactly, character for character\n" +
	"\n" +
	"Output only the (possibly redacted) text with no preamble, explanation, or additional formatting.\n\n"

// piiDriftThreshold is the minimum ratio of output-to-input rune count that is
// considered plausible for a redaction pass. If the output is shorter than this
// fraction of the input, the model likely paraphrased or dropped content rather
// than redacting inline, and the result is rejected.
const piiDriftThreshold = 0.70

// checkPIIDrift returns an error if screened is implausibly shorter than original,
// indicating the model may have paraphrased rather than performed inline redaction.
// Exported for testing without a live API call.
func checkPIIDrift(original, screened string) error {
	origRunes := len([]rune(original))
	if origRunes == 0 {
		return nil
	}
	screenedRunes := len([]rune(screened))
	if float64(screenedRunes) < float64(origRunes)*piiDriftThreshold {
		return fmt.Errorf(
			"pii screen: output (%d runes) is implausibly shorter than input (%d runes); "+
				"model may have paraphrased rather than redacted inline",
			screenedRunes, origRunes,
		)
	}
	return nil
}

// ScreenPII returns text with PII redacted in place. Phone numbers, home/street
// addresses, email addresses (personal and professional), government IDs, and dates of
// birth are replaced with [redacted]. LinkedIn and GitHub URLs are preserved.
//
// Unlike Describe, the full text is passed without truncation — partial coverage
// would leave PII past the cutoff unredacted.
func (c *Client) ScreenPII(ctx context.Context, text string) (string, error) {
	result, err := c.generateContent(ctx, piiScreenModel, []*genai.Part{
		genai.NewPartFromText(piiScreenPrompt + text),
	})
	if err != nil {
		return "", fmt.Errorf("screen pii: %w", err)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return "", fmt.Errorf("screen pii: model returned empty output (content may have been filtered)")
	}
	if err := checkPIIDrift(text, result); err != nil {
		return "", err
	}
	return result, nil
}

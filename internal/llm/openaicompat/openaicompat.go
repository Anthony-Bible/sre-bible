// Package openaicompat implements the rag.FollowUpSuggester port against any
// OpenAI-compatible chat-completions endpoint (OpenRouter, vLLM, Ollama's /v1,
// LM Studio, …) using the official github.com/openai/openai-go SDK pointed at a
// custom base URL. It is wired in only for the follow-up suggestion cards; the main
// chat path stays Anthropic-native. The port returns a plain []string, so nothing
// provider-specific leaks past this package.
package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// Suggester generates follow-up question suggestions via an OpenAI-compatible
// chat-completions endpoint. It implements rag.FollowUpSuggester.
type Suggester struct {
	client openai.Client
	model  string
	log    *slog.Logger
}

// New builds a Suggester pointed at baseURL (any OpenAI-compatible /v1 endpoint).
// apiKey may be empty for local servers that need no auth — in that case the
// Authorization header is stripped entirely. This is deliberate: the openai-go SDK
// falls back to the ambient OPENAI_API_KEY environment variable when no key is passed,
// so we install a middleware that deletes the header after the SDK applies it, honouring
// the "no auth" contract instead of silently leaking an env key to a third-party
// endpoint. extraBody, when non-nil, merges each top-level key/value into every request
// body via the SDK's WithJSONSet — the provider-neutral escape hatch for non-standard
// fields, e.g. disabling a thinking model's reasoning so a tight token cap is not consumed
// before any content is produced (GLM: {"thinking":{"type":"disabled"}}; OpenRouter:
// {"reasoning":{"enabled":false}}). model is the model ID; log may be nil (slog.Default()).
func New(baseURL, apiKey, model string, extraBody map[string]any, log *slog.Logger) *Suggester {
	if log == nil {
		log = slog.Default()
	}
	opts := []option.RequestOption{option.WithBaseURL(baseURL)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	} else {
		opts = append(opts, option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			r.Header.Del("Authorization")
			return next(r)
		}))
	}
	for k, v := range extraBody {
		opts = append(opts, option.WithJSONSet(k, v))
	}
	return &Suggester{
		client: openai.NewClient(opts...),
		model:  model,
		log:    log,
	}
}

// SuggestFollowUps sends the system prompt plus the recent conversation to the
// configured endpoint with response_format=json_object and returns up to maxQuestions
// trimmed, non-empty questions. It parses a {"questions":[...]} object, falling back to
// a bare JSON array for servers that ignore response_format. Any transport, status, or
// parse error is returned so the caller can degrade to "no cards".
func (s *Suggester) SuggestFollowUps(ctx context.Context, systemPrompt string, messages []rag.Message, maxQuestions int) ([]string, error) {
	oaiMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	oaiMessages = append(oaiMessages, openai.SystemMessage(systemPrompt))
	for _, m := range messages {
		switch m.Role {
		case rag.RoleUser:
			oaiMessages = append(oaiMessages, openai.UserMessage(m.Content))
		case rag.RoleAssistant:
			oaiMessages = append(oaiMessages, openai.AssistantMessage(m.Content))
		}
	}

	params := openai.ChatCompletionNewParams{
		Model:     s.model,
		Messages:  oaiMessages,
		MaxTokens: openai.Int(rag.MaxFollowUpTokens),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
	}

	resp, err := s.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openaicompat suggest: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openaicompat suggest: response had no choices")
	}
	choice := resp.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" {
		// A blank completion is not a parse failure: most often a thinking model spent the
		// whole MaxTokens budget on reasoning before emitting content (finish_reason="length").
		// Degrade to "no cards" rather than erroring, but log finish_reason/refusal so a
		// misconfigured token budget is diagnosable instead of surfacing as an opaque parse
		// error. Disable the model's reasoning via extraBody (see New) to fix the root cause.
		s.log.WarnContext(ctx, "openaicompat suggest: empty completion, no cards",
			slog.String("finish_reason", choice.FinishReason),
			slog.String("refusal", choice.Message.Refusal))
		return nil, nil
	}
	return parseQuestions(choice.Message.Content, maxQuestions)
}

// parseQuestions reads the model's JSON content string. It first tries the
// {"questions":[...]} object shape; if that does not unmarshal, it falls back to a bare
// ["..."] array (servers that honour the prompt but ignore response_format). It trims
// blanks and caps to maxQuestions, returning an error only when neither shape parses.
func parseQuestions(content string, maxQuestions int) ([]string, error) {
	var obj struct {
		Questions []string `json:"questions"`
	}
	if err := json.Unmarshal([]byte(content), &obj); err == nil {
		return rag.CapQuestions(obj.Questions, maxQuestions), nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(content), &arr); err == nil {
		return rag.CapQuestions(arr, maxQuestions), nil
	}
	// Neither shape parsed. A truncated payload (a verbose model that over-generated and
	// ran past MaxTokens, finish_reason="length") lands here too — deliberately. The
	// feature degrades to no cards on any error, and surfacing it as an error keeps the
	// misconfiguration visible (logged with this content, metered status=error) rather
	// than silently papering over it. Disable the model's reasoning via extraBody (see New).
	return nil, fmt.Errorf("openaicompat suggest: unparseable content %q", content)
}

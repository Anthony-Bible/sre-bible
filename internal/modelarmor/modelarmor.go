// Package modelarmor wraps Google Cloud Model Armor's SanitizeUserPrompt API to
// screen inbound Viewer prompts for prompt-injection and jailbreak attempts
// before they reach embedding or generation. It is a detection gate — it reports
// whether a prompt matched a template filter and never mutates the prompt text.
package modelarmor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	modelarmor "cloud.google.com/go/modelarmor/apiv1"
	modelarmorpb "cloud.google.com/go/modelarmor/apiv1/modelarmorpb"
	"google.golang.org/api/option"
)

// Client wraps the Model Armor SDK client, bound to a single sanitization template.
type Client struct {
	inner    *modelarmor.Client
	template string
	log      *slog.Logger
}

// NewClient constructs a Model Armor Client bound to the given template resource
// name (projects/<p>/locations/<loc>/templates/<id>). It derives the region from
// the template name, targets the matching regional endpoint (Model Armor has no
// global endpoint), and authenticates via Application Default Credentials (ADC) —
// distinct from Gemini, which uses an API key.
func NewClient(ctx context.Context, template string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	location, err := locationFromTemplate(template)
	if err != nil {
		return nil, fmt.Errorf("model armor template: %w", err)
	}
	inner, err := modelarmor.NewClient(ctx, option.WithEndpoint(regionalEndpoint(location)))
	if err != nil {
		return nil, fmt.Errorf("create model armor client: %w", err)
	}
	return &Client{inner: inner, template: template, log: log}, nil
}

// SanitizePrompt screens prompt against the configured template. blocked is true
// when any configured filter matched (overall MATCH_FOUND); reason names the
// matched filters for logging. A non-nil err signals an API/transport failure —
// the caller decides the availability posture (the RAG pipeline fails open).
func (c *Client) SanitizePrompt(ctx context.Context, prompt string) (bool, string, error) {
	resp, err := c.inner.SanitizeUserPrompt(ctx, &modelarmorpb.SanitizeUserPromptRequest{
		Name: c.template,
		UserPromptData: &modelarmorpb.DataItem{
			DataItem: &modelarmorpb.DataItem_Text{Text: prompt},
		},
	})
	if err != nil {
		return false, "", fmt.Errorf("sanitize user prompt: %w", err)
	}
	blocked, reason := interpretVerdict(resp.GetSanitizationResult())
	c.log.DebugContext(ctx, "model armor sanitize result",
		slog.Bool("blocked", blocked),
		slog.String("reason", reason),
	)
	return blocked, reason, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

// regionalEndpoint returns the Model Armor regional endpoint (host:port) for a
// location. Model Armor requires a regional endpoint; there is no global one.
func regionalEndpoint(location string) string {
	return fmt.Sprintf("modelarmor.%s.rep.googleapis.com:443", location)
}

// locationFromTemplate extracts <loc> from a template resource name of the form
// projects/<p>/locations/<loc>/templates/<id>. It errors on a malformed name or
// an empty location segment.
func locationFromTemplate(name string) (string, error) {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "locations" {
			loc := parts[i+1]
			if loc == "" {
				return "", fmt.Errorf("empty location in template %q", name)
			}
			return loc, nil
		}
	}
	return "", fmt.Errorf("no locations segment in template %q", name)
}

// interpretVerdict reads a SanitizationResult into a block decision and a
// human-readable reason. blocked mirrors the overall FilterMatchState; reason is
// the sorted, comma-joined names of the individual filters that matched (logs only).
func interpretVerdict(res *modelarmorpb.SanitizationResult) (bool, string) {
	if res == nil {
		return false, ""
	}
	blocked := res.GetFilterMatchState() == modelarmorpb.FilterMatchState_MATCH_FOUND
	matched := make([]string, 0, len(res.GetFilterResults()))
	for name, fr := range res.GetFilterResults() {
		if filterResultMatched(fr) {
			matched = append(matched, name)
		}
	}
	sort.Strings(matched)
	return blocked, strings.Join(matched, ",")
}

// filterResultMatched reports whether a single filter's result is MATCH_FOUND.
// Each accessor returns nil for an unset oneof variant; proto getters are
// nil-safe, so an unset variant reports UNSPECIFIED (i.e. not matched).
func filterResultMatched(fr *modelarmorpb.FilterResult) bool {
	if fr == nil {
		return false
	}
	const matched = modelarmorpb.FilterMatchState_MATCH_FOUND
	if fr.GetRaiFilterResult().GetMatchState() == matched ||
		fr.GetPiAndJailbreakFilterResult().GetMatchState() == matched ||
		fr.GetMaliciousUriFilterResult().GetMatchState() == matched ||
		fr.GetCsamFilterFilterResult().GetMatchState() == matched ||
		fr.GetVirusScanFilterResult().GetMatchState() == matched {
		return true
	}
	// SDP reports its state in a nested inspect / deidentify result.
	if sdp := fr.GetSdpFilterResult(); sdp != nil {
		return sdp.GetInspectResult().GetMatchState() == matched ||
			sdp.GetDeidentifyResult().GetMatchState() == matched
	}
	return false
}

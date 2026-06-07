package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// Verifier calls the Cloudflare Turnstile siteverify API.
type Verifier struct {
	secret   string
	endpoint string
	client   *http.Client
	log      *slog.Logger
}

// NewVerifier creates a Verifier with a 10-second HTTP timeout.
func NewVerifier(secret string, log *slog.Logger) *Verifier {
	if log == nil {
		log = slog.Default()
	}
	return &Verifier{
		secret:   secret,
		endpoint: defaultEndpoint,
		client:   &http.Client{Timeout: 10 * time.Second},
		log:      log,
	}
}

// SetEndpoint overrides the siteverify URL. Used in tests to point at a local httptest.Server.
func (v *Verifier) SetEndpoint(u string) {
	v.endpoint = u
}

type verifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify calls the siteverify API and returns (true, nil) when the token is valid.
// Any network, non-200, or decode error returns (false, err) — fail closed.
func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) (bool, error) {
	body := url.Values{
		"secret":   {v.secret},
		"response": {token},
	}
	if remoteIP != "" {
		body.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return false, fmt.Errorf("turnstile build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(req)
	if err != nil {
		v.log.ErrorContext(ctx, "turnstile request failed", slog.Any("err", err))
		return false, fmt.Errorf("turnstile request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		v.log.ErrorContext(ctx, "turnstile non-200 response", slog.Int("status", resp.StatusCode))
		return false, fmt.Errorf("turnstile: unexpected status %d", resp.StatusCode)
	}

	var result verifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		v.log.ErrorContext(ctx, "turnstile decode failed", slog.Any("err", err))
		return false, fmt.Errorf("turnstile decode: %w", err)
	}

	if !result.Success {
		v.log.InfoContext(ctx, "turnstile verification failed", slog.Any("error-codes", result.ErrorCodes))
	}
	return result.Success, nil
}

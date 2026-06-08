package gemini

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"google.golang.org/genai"
)

const (
	maxEmbedRetries = 3
	maxRetryDelay   = 8 * time.Second
)

// retryBaseDelay is the first backoff sleep on a rate-limited retry. It is a var
// (not a const) so tests can shrink it to keep retry_test.go fast.
var retryBaseDelay = time.Second

// isRateLimited reports whether err is a Gemini quota / rate-limit error that is
// safe to retry. It unwraps to genai.APIError and matches either HTTP 429 or the
// gRPC-style "RESOURCE_EXHAUSTED" status — the SDK populates both fields, and
// matching either is robust to upstream changes.
func isRateLimited(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == 429 || apiErr.Status == "RESOURCE_EXHAUSTED"
}

// retryEmbed runs fn with bounded exponential backoff on rate-limit errors.
// Non-rate-limit errors and successful results return immediately. The sleep
// respects ctx cancellation. op is included in retry log lines for grep-ability.
func retryEmbed[T any](ctx context.Context, log *slog.Logger, op string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}
		if !isRateLimited(err) || attempt >= maxEmbedRetries {
			return zero, err
		}
		delay := backoffDelay(attempt)
		if log != nil {
			log.WarnContext(ctx, "gemini rate limited, retrying",
				"op", op,
				"attempt", attempt+1,
				"max_attempts", maxEmbedRetries,
				"delay_ms", delay.Milliseconds(),
			)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return zero, ctx.Err()
		case <-t.C:
		}
	}
}

// backoffDelay returns retryBaseDelay * 2^attempt plus up to 25% jitter, capped
// at maxRetryDelay.
func backoffDelay(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d <= 0 || d > maxRetryDelay {
		d = maxRetryDelay
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	return d + jitter
}

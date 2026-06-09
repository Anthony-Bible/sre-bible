package gemini

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genai"
)

func TestIsRateLimited(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"api 500", genai.APIError{Code: 500, Status: "INTERNAL"}, false},
		{"api 429", genai.APIError{Code: 429, Message: "rate"}, true},
		{"api resource exhausted by status", genai.APIError{Status: "RESOURCE_EXHAUSTED"}, true},
		{"wrapped 429", fmt.Errorf("embed query: %w", genai.APIError{Code: 429}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isRateLimited(tc.err); got != tc.want {
				t.Fatalf("isRateLimited(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryEmbed_SucceedsFirstTry(t *testing.T) {
	t.Parallel()
	calls := 0
	got, err := retryEmbed(context.Background(), nil, "op", func(ctx context.Context) (int, error) {
		calls++
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 || calls != 1 {
		t.Fatalf("got=%d calls=%d, want 42/1", got, calls)
	}
}

func TestRetryEmbed_RetriesThenSucceeds(t *testing.T) {
	swapBaseDelay(t, time.Millisecond)
	calls := 0
	got, err := retryEmbed(context.Background(), nil, "op", func(ctx context.Context) (int, error) {
		calls++
		if calls < 3 {
			return 0, genai.APIError{Code: 429}
		}
		return 7, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 || calls != 3 {
		t.Fatalf("got=%d calls=%d, want 7/3", got, calls)
	}
}

func TestRetryEmbed_GivesUpAfterMaxAttempts(t *testing.T) {
	swapBaseDelay(t, time.Millisecond)
	calls := 0
	_, err := retryEmbed(context.Background(), nil, "op", func(ctx context.Context) (int, error) {
		calls++
		return 0, genai.APIError{Code: 429}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isRateLimited(err) {
		t.Fatalf("expected rate-limited error, got %v", err)
	}
	if calls != maxEmbedRetries+1 {
		t.Fatalf("calls=%d, want %d", calls, maxEmbedRetries+1)
	}
}

func TestRetryEmbed_NonRateLimitReturnsImmediately(t *testing.T) {
	t.Parallel()
	calls := 0
	wantErr := errors.New("oops")
	_, err := retryEmbed(context.Background(), nil, "op", func(ctx context.Context) (int, error) {
		calls++
		return 0, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err=%v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestRetryEmbed_ContextCanceledDuringBackoff(t *testing.T) {
	swapBaseDelay(t, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	_, err := retryEmbed(ctx, nil, "op", func(ctx context.Context) (int, error) {
		return 0, genai.APIError{Code: 429}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got err=%v, want context.Canceled", err)
	}
}

// swapBaseDelay temporarily shrinks retryBaseDelay for fast tests, restoring it
// on cleanup. Tests that use this cannot run with t.Parallel since the var is
// package-global.
func swapBaseDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := retryBaseDelay
	retryBaseDelay = d
	t.Cleanup(func() { retryBaseDelay = orig })
}

package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewHandlerSelection(t *testing.T) {
	t.Run("json format emits parseable JSON", func(t *testing.T) {
		var buf bytes.Buffer
		logger := New(&buf, "json", slog.LevelInfo)
		logger.Info("hello", "k", "v")

		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("expected JSON output, got %q: %v", buf.String(), err)
		}
		if rec["msg"] != "hello" || rec["k"] != "v" {
			t.Fatalf("unexpected JSON record: %v", rec)
		}
	})

	for _, format := range []string{"text", "", "garbage"} {
		t.Run("non-json format ("+format+") emits text", func(t *testing.T) {
			var buf bytes.Buffer
			logger := New(&buf, format, slog.LevelInfo)
			logger.Info("hello", "k", "v")

			out := buf.String()
			if json.Valid(bytes.TrimSpace(buf.Bytes())) {
				t.Fatalf("expected text output, got JSON: %q", out)
			}
			if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
				t.Fatalf("unexpected text output: %q", out)
			}
		})
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "text", slog.LevelWarn)

	logger.Info("below threshold")
	if buf.Len() != 0 {
		t.Fatalf("expected Info to be suppressed at Warn level, got %q", buf.String())
	}

	logger.Warn("at threshold")
	if !strings.Contains(buf.String(), "at threshold") {
		t.Fatalf("expected Warn to be emitted, got %q", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

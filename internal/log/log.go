// Package log centralizes construction of the root application *slog.Logger.
//
// It is a composition-root helper, invoked only from each cmd/ main(), so that
// LOG_FORMAT and LOG_LEVEL behave identically across every binary. It introduces
// no global logger and no port: internal packages keep receiving an injected
// *slog.Logger as before — only the construction is shared here.
package log

import (
	"io"
	"log/slog"
	"strings"
)

// New builds the root application logger writing to w. A format of "json" selects
// slog's JSONHandler; any other value (including "") selects the TextHandler.
func New(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// ParseLevel maps a LOG_LEVEL string (case-insensitive: debug, info, warn, error)
// to an slog.Level, defaulting to Info for empty or unrecognized values.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

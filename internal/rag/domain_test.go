package rag_test

import (
	"context"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

type customKey string

func TestPersonaMode_ContextHelpers(t *testing.T) {
	t.Parallel()

	// Default context should return ModeStandard
	bgCtx := context.Background()
	if got := rag.PersonaModeFromContext(bgCtx); got != rag.ModeStandard {
		t.Errorf("default PersonaModeFromContext = %q, want %q", got, rag.ModeStandard)
	}

	// Setting ModeDeadpool should retrieve ModeDeadpool
	dpCtx := rag.WithPersonaMode(bgCtx, rag.ModeDeadpool)
	if got := rag.PersonaModeFromContext(dpCtx); got != rag.ModeDeadpool {
		t.Errorf("PersonaModeFromContext(dpCtx) = %q, want %q", got, rag.ModeDeadpool)
	}

	// Setting ModeStandard should retrieve ModeStandard
	stdCtx := rag.WithPersonaMode(bgCtx, rag.ModeStandard)
	if got := rag.PersonaModeFromContext(stdCtx); got != rag.ModeStandard {
		t.Errorf("PersonaModeFromContext(stdCtx) = %q, want %q", got, rag.ModeStandard)
	}

	// Invalid value in context should fallback to ModeStandard
	invalidCtx := context.WithValue(bgCtx, customKey("persona_mode"), "unsupported-mode-value")
	if got := rag.PersonaModeFromContext(invalidCtx); got != rag.ModeStandard {
		t.Errorf("PersonaModeFromContext(invalidCtx) = %q, want %q", got, rag.ModeStandard)
	}
}

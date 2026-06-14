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

func TestPersonaModeFromContext_Interview(t *testing.T) {
	t.Parallel()

	bg := context.Background()

	// Absent flag → false.
	if rag.InterviewModeFromContext(bg) {
		t.Error("InterviewModeFromContext(background) = true, want false")
	}

	// Flag on → persona resolves to ModeInterview.
	onCtx := rag.WithInterviewMode(bg, true)
	if !rag.InterviewModeFromContext(onCtx) {
		t.Error("InterviewModeFromContext(on) = false, want true")
	}
	if got := rag.PersonaModeFromContext(onCtx); got != rag.ModeInterview {
		t.Errorf("PersonaModeFromContext(interview on) = %q, want %q", got, rag.ModeInterview)
	}

	// Flag on supersedes a directly-set Deadpool persona.
	dpOn := rag.WithInterviewMode(rag.WithPersonaMode(bg, rag.ModeDeadpool), true)
	if got := rag.PersonaModeFromContext(dpOn); got != rag.ModeInterview {
		t.Errorf("interview flag must supersede Deadpool: got %q, want %q", got, rag.ModeInterview)
	}

	// Flag off → the underlying persona is honoured.
	dpOff := rag.WithInterviewMode(rag.WithPersonaMode(bg, rag.ModeDeadpool), false)
	if got := rag.PersonaModeFromContext(dpOff); got != rag.ModeDeadpool {
		t.Errorf("interview flag off must yield underlying persona: got %q, want %q", got, rag.ModeDeadpool)
	}
}

package rag_test

import (
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

func TestInterviewPersona_Copy(t *testing.T) {
	t.Parallel()

	persona := rag.DefaultPersonas()[rag.ModeInterview]
	if persona == "" {
		t.Fatal("DefaultPersonas()[ModeInterview] is empty")
	}
	if persona != rag.InterviewPersona {
		t.Error("DefaultPersonas()[ModeInterview] must equal rag.InterviewPersona")
	}

	lower := strings.ToLower(persona)

	// Forbidden copy: the persona must never compute an aggregate/pass-fail,
	// reference the email/scheduling funnel, refuse general questions, or leak
	// the old gated-barrier framing.
	forbidden := []string{
		"you failed",
		"70%",
		"calendly",
		"send_contact_email",
		"ignore general questions",
		"sre barrier",
	}
	for _, f := range forbidden {
		if strings.Contains(lower, f) {
			t.Errorf("InterviewPersona must NOT contain forbidden substring %q", f)
		}
	}

	// Required content: it must drive the grading tool, link the repo, and walk
	// all three scenarios.
	required := []string{
		"evaluate_interview_answer",
		"github.com/Anthony-Bible/sre-bible",
	}
	for _, r := range required {
		if !strings.Contains(persona, r) {
			t.Errorf("InterviewPersona must contain %q", r)
		}
	}
	for _, sc := range []string{"scenario 1", "scenario 2", "scenario 3"} {
		if !strings.Contains(lower, sc) {
			t.Errorf("InterviewPersona must reference %q", sc)
		}
	}
}

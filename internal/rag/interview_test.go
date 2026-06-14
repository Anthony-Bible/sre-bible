package rag

import (
	"encoding/json"
	"testing"
)

// TestInterviewEvaluation_JSONShape locks the wire shape the LLM tool result
// commits to. Renaming any of these JSON keys is a breaking change for the
// evaluate_interview_answer tool contract and must be done deliberately.
func TestInterviewEvaluation_JSONShape(t *testing.T) {
	t.Parallel()
	e := InterviewEvaluation{
		Score:                85,
		Feedback:             "great",
		Passed:               true,
		ConceptsDemonstrated: []string{"singleflight"},
	}
	blob, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"score", "feedback", "passed", "concepts_demonstrated"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing required key %q in %s", k, blob)
		}
	}
}

func TestInterviewScenarioConstants(t *testing.T) {
	t.Parallel()
	if InterviewNumScenarios != 3 {
		t.Errorf("InterviewNumScenarios: got %d, want 3", InterviewNumScenarios)
	}
	if InterviewScenarioCascadeCacheStampede != 0 ||
		InterviewScenarioBGPDNS != 1 ||
		InterviewScenarioServerlessColdStart != 2 {
		t.Error("interview scenario indices must be 0,1,2 — they are part of the tool contract")
	}
}

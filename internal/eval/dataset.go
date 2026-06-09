package eval

import (
	"encoding/json"
	"fmt"
	"os"
)

// Category classifies the nature of a golden eval case.
type Category string

const (
	CategoryGroundedFactual Category = "grounded_factual"
	CategoryRetrievalCheck  Category = "retrieval_check"
	CategoryRefusal         Category = "refusal"
	CategoryContactFlow     Category = "contact_flow"
)

// GoldenCase is a single labelled example used for evaluation.
type GoldenCase struct {
	ID                  string   `json:"id"`
	Category            Category `json:"category"`
	Question            string   `json:"question"`
	ExpectedSourceNames []string `json:"expected_source_names,omitempty"`
	MustNotContain      []string `json:"must_not_contain,omitempty"`
	ExpectedRefusal     bool     `json:"expected_refusal,omitempty"`
	ExpectedToolCalls   []string `json:"expected_tool_calls,omitempty"`
	JudgeRubric         string   `json:"judge_rubric,omitempty"`
}

// Dataset is the top-level container for golden eval cases.
type Dataset struct {
	Cases []GoldenCase `json:"cases"`
}

// RetrievedChunkRecord holds a single chunk that was retrieved during evaluation.
type RetrievedChunkRecord struct {
	Content    string `json:"content"`
	SourceName string `json:"source_name"`
}

// Result captures everything the pipeline produced for a single GoldenCase run.
type Result struct {
	Case            GoldenCase
	Answer          string
	Citations       []string
	RetrievedChunks []RetrievedChunkRecord
	ToolCallsSeen   []string
	Error           error
}

// ScoreDetail holds the individual scoring signals for an EvalResult.
type ScoreDetail struct {
	RecallScore  float64 `json:"recall_score"`
	RefusalPass  bool    `json:"refusal_pass"`
	MustNotPass  bool    `json:"must_not_pass"`
	GroundScore  float64 `json:"ground_score"`
	JudgeSkipped bool    `json:"judge_skipped"`
}

// ScoredResult pairs a Result with its computed score.
type ScoredResult struct {
	Result Result
	Score  ScoreDetail
	Pass   bool
	Notes  string
}

// LoadDataset reads a JSON file at path and returns the parsed Dataset.
// It returns an error if the file cannot be read, if JSON is malformed,
// if the dataset contains no cases, or if any case has an unknown category.
func LoadDataset(path string) (*Dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("LoadDataset: reading %q: %w", path, err)
	}

	var ds Dataset
	if err := json.Unmarshal(data, &ds); err != nil {
		return nil, fmt.Errorf("LoadDataset: unmarshalling %q: %w", path, err)
	}

	if len(ds.Cases) == 0 {
		return nil, fmt.Errorf("LoadDataset: dataset %q contains no cases", path)
	}

	// Reject unknown category values at the boundary. An unrecognised category
	// (e.g. a typo) is otherwise silently dropped during aggregation, so the
	// mis-categorised case is never evaluated and its absence raises no signal.
	for i, c := range ds.Cases {
		switch c.Category {
		case CategoryGroundedFactual, CategoryRetrievalCheck, CategoryRefusal, CategoryContactFlow:
			// valid
		default:
			return nil, fmt.Errorf("LoadDataset: case %d (id %q) has unknown category %q", i, c.ID, c.Category)
		}
	}

	return &ds, nil
}

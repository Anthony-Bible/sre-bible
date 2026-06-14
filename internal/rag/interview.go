package rag

import "context"

// InterviewState captures the persistent progress of an interview simulator
// session — the questions asked, the candidate's answers, per-answer scoring,
// and the running totals/outcome flags.
type InterviewState struct {
	CurrentQuestionIndex int      `json:"current_question_index"`
	TotalQuestions       int      `json:"total_questions"`
	Questions            []string `json:"questions"`
	Answers              []string `json:"answers"`
	Scores               []int    `json:"scores"`
	Feedbacks            []string `json:"feedbacks"`
	Completed            bool     `json:"completed"`
	Passed               bool     `json:"passed"`
	TotalScore           int      `json:"total_score"`
}

// InterviewStateStore persists per-session interview simulator progress.
// Implemented by db.SessionStore.
type InterviewStateStore interface {
	GetInterviewState(ctx context.Context, sessionID string) (*InterviewState, error)
	SetInterviewState(ctx context.Context, sessionID string, state *InterviewState) error
	ClearInterviewState(ctx context.Context, sessionID string) error
	IsInterviewActive(ctx context.Context, sessionID string) (bool, error)
}

// Interview scenario indices. The evaluate_interview_answer tool only accepts
// question_index values in [0, InterviewNumScenarios).
const (
	InterviewScenarioCascadeCacheStampede = 0
	InterviewScenarioBGPDNS               = 1
	InterviewScenarioServerlessColdStart  = 2
	InterviewNumScenarios                 = 3
)

// ToolEvaluateInterviewAnswer is the name of the per-answer grading tool the
// interview persona invokes once per scenario. It is the single source of truth
// for the tool name: internal/llm advertises/dispatches under it, and the server
// recognises it in the trace stream to advance the HUD scenario counter.
const ToolEvaluateInterviewAnswer = "evaluate_interview_answer"

// interviewScenarioTitles are the short, HUD-friendly labels for the three fixed
// scenarios, indexed by the InterviewScenario* constants. They mirror the verbatim
// candidate-facing scenarios authored in InterviewPersona (prompt.go) — these are
// titles for state/UI, the persona holds the full wording the model presents.
var interviewScenarioTitles = []string{
	InterviewScenarioCascadeCacheStampede: "Cascading Failure / Cache Stampede",
	InterviewScenarioBGPDNS:               "BGP Route Leak / DNS Hijack",
	InterviewScenarioServerlessColdStart:  "Serverless Cold Starts & DB Connection Exhaustion",
}

// NewInterviewState returns the pre-seeded interview state for a freshly activated
// session: the three fixed scenarios, the counter at the first scenario, and empty
// per-answer slices. Used by the server on first flip-on (and on restart).
func NewInterviewState() *InterviewState {
	titles := make([]string, len(interviewScenarioTitles))
	copy(titles, interviewScenarioTitles)
	return &InterviewState{
		CurrentQuestionIndex: 0,
		TotalQuestions:       InterviewNumScenarios,
		Questions:            titles,
	}
}

// InterviewEvaluation is the structured result of grading one interview answer.
// It is returned by the Judge as the tool result for evaluate_interview_answer.
// Score is clamped to [0,100]; Passed is derived from Score (>=60).
type InterviewEvaluation struct {
	Score                int      `json:"score"`
	Feedback             string   `json:"feedback"`
	Passed               bool     `json:"passed"`
	ConceptsDemonstrated []string `json:"concepts_demonstrated"`
}

// Judge grades a single interview answer against the rubric for the given
// scenario index, returning a structured InterviewEvaluation. Implementations
// typically issue a structured Claude Haiku call. The Judge MUST NOT log or
// persist the raw user answer.
type Judge interface {
	EvaluateAnswer(ctx context.Context, questionIdx int, questionText, userAnswer string) (*InterviewEvaluation, error)
}

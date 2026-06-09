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

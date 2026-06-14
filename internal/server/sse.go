package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

type tokenPayload struct {
	T string `json:"t"`
}

type donePayload struct {
	Citations []string `json:"citations"`
}

type msgPayload struct {
	Msg string `json:"msg"`
}

// tracePayload wraps a single Agent Trace step for the "trace" SSE event.
type tracePayload struct {
	Step rag.TraceStep `json:"step"`
}

// interviewProgressPayload carries the HUD scenario counter for the
// "interview_progress" SSE event: Current is the number of scenarios graded so
// far (1-based), Total the number of scenarios in the run. Deliberately no
// score/pass field — the rescope (EPIC #26) drops all aggregate outcome.
type interviewProgressPayload struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

// writeSSE writes "event: <name>\ndata: <json>\n\n" and flushes.
func writeSSE(w http.ResponseWriter, f http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal SSE payload: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("write SSE frame: %w", err)
	}
	f.Flush()
	return nil
}

// sseToken sends a single token to the client.
func sseToken(w http.ResponseWriter, f http.Flusher, token string) error {
	return writeSSE(w, f, "token", tokenPayload{T: token})
}

// sseDone sends the completion event with deduplicated citations.
// A nil slice is normalised to an empty slice so the client always receives an array.
func sseDone(w http.ResponseWriter, f http.Flusher, citations []string) error {
	if citations == nil {
		citations = []string{}
	}
	return writeSSE(w, f, "done", donePayload{Citations: citations})
}

// sseInterviewProgress sends the interview HUD counter after an answer is graded,
// so the frontend can update its "Scenario N of M" indicator live.
func sseInterviewProgress(w http.ResponseWriter, f http.Flusher, current, total int) error {
	return writeSSE(w, f, "interview_progress", interviewProgressPayload{Current: current, Total: total})
}

// sseError sends an error event to the client.
func sseError(w http.ResponseWriter, f http.Flusher, msg string) error {
	return writeSSE(w, f, "error", msgPayload{Msg: msg})
}

// sseTrace sends a single Agent Trace step to the client as it is produced, so the
// browser can render the live trace timeline incrementally. The same steps are also
// persisted with the assistant turn, so the trace survives reload.
func sseTrace(w http.ResponseWriter, f http.Flusher, step rag.TraceStep) error {
	return writeSSE(w, f, "trace", tracePayload{Step: step})
}

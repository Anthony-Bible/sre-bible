package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type tokenPayload struct {
	T string `json:"t"`
}

type donePayload struct {
	Citations []string `json:"citations"`
}

type errorPayload struct {
	Msg string `json:"msg"`
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

// sseError sends an error event to the client.
func sseError(w http.ResponseWriter, f http.Flusher, msg string) error {
	return writeSSE(w, f, "error", errorPayload{Msg: msg})
}

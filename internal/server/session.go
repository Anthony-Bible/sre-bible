package server

import (
	"net/http"

	"github.com/google/uuid"
)

const sessionHeader = "X-Session-Id"

// sessionFromRequest returns the session ID from the X-Session-Id header, or "" if absent.
func sessionFromRequest(r *http.Request) string {
	return r.Header.Get(sessionHeader)
}

// validSessionID reports whether id is a valid UUID.
func validSessionID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// requireSession extracts and validates the session ID from the request.
// It writes 400 to w and returns ("", false) if the ID is absent or malformed.
func requireSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	sid := sessionFromRequest(r)
	if !validSessionID(sid) {
		http.Error(w, "invalid session ID", http.StatusBadRequest)
		return "", false
	}
	return sid, true
}

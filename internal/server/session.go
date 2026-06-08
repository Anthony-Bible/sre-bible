package server

import (
	"net/http"
	"regexp"
)

const sessionHeader = "X-Session-Id"

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// sessionFromRequest returns the session ID from the X-Session-Id header, or "" if absent.
func sessionFromRequest(r *http.Request) string {
	return r.Header.Get(sessionHeader)
}

// validSessionID reports whether id is a valid UUID.
func validSessionID(id string) bool {
	return uuidV4Re.MatchString(id)
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

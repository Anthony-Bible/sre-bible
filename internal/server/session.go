package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
)

const (
	cookieName   = "session_id"
	cookieMaxAge = 30 * 24 * 60 * 60 // 30 days in seconds
)

// newSessionID generates a UUID v4 using crypto/rand.
// Reads 16 bytes, sets version (b[6]) and variant (b[8]) bits,
// and formats as 8-4-4-4-12 hex with dashes.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

// sessionFromRequest returns the session ID from the cookie, or "" if absent.
func sessionFromRequest(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookie writes an HttpOnly, SameSite=Lax session cookie to w.
func setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
	})
}

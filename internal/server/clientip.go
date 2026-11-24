package server

import (
	"net"
	"net/http"
	"net/netip"
)

// clientIP returns the best-available client IP string for the request: the
// Cloudflare-supplied Cf-Connecting-Ip header, falling back to r.RemoteAddr.
//
// The origin is locked to Cloudflare-only reachability (ADR 0016), so behind the
// edge Cf-Connecting-Ip is the real visitor IP and is trustworthy; the fallback
// matters only for local/direct dev where the header is absent. Returned raw
// (RemoteAddr may carry a :port) — callers that need a normalized rate-limit key
// use clientIPKey instead.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// clientIPKey returns the per-IP rate-limit key for the request, or "" if no IP
// can be parsed (in which case the caller fails open to the per-session
// dimension rather than lumping every unparseable request into one bucket).
func clientIPKey(r *http.Request) string {
	return normalizeIPKey(clientIP(r))
}

// normalizeIPKey turns a client IP string into a stable rate-limit key:
//
//   - IPv4 (including an IPv4-mapped IPv6 address) → the dotted-quad address.
//   - IPv6 → the /64 prefix, not the full /128. A single end user is routinely
//     assigned a whole /64, while an attacker with one /64 controls billions of
//     distinct /128s; keying on /64 throttles the entity that actually maps to a
//     subscriber and denies the trivial per-/128 bypass.
//
// A trailing ":port" (as in r.RemoteAddr) is stripped first. The input is never
// logged or persisted — it is used only as an in-memory limiter key — so an
// unparseable value simply yields "" and the caller skips the per-IP check.
func normalizeIPKey(ip string) string {
	if ip == "" {
		return ""
	}
	// Strip a host:port form (r.RemoteAddr) if present; Cf-Connecting-Ip is bare.
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	// Collapse IPv4-mapped IPv6 (::ffff:a.b.c.d) to plain IPv4 so the two forms of
	// the same address share one bucket and v4 is never keyed as a /64.
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		// Prefix only errors on an impossible bit length; treat as unkeyable.
		return ""
	}
	return prefix.String()
}

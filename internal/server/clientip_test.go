package server

import "testing"

// TestNormalizeIPKey covers the IP → rate-limit-key normalization: IPv4 is kept
// as-is, IPv6 collapses to its /64 prefix (so addresses sharing a /64 share a
// bucket but different /64s do not), IPv4-mapped IPv6 unmaps to plain IPv4, a
// host:port form is stripped, and anything unparseable yields "".
func TestNormalizeIPKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4", "203.0.113.7", "203.0.113.7"},
		{"ipv4 with port", "203.0.113.7:54321", "203.0.113.7"},
		{"ipv4-mapped ipv6 unmaps", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6 to /64", "2001:db8:abcd:1234:5678:9abc:def0:1111", "2001:db8:abcd:1234::/64"},
		{"ipv6 with port and zone-free brackets", "[2001:db8:abcd:1234::1]:443", "2001:db8:abcd:1234::/64"},
		{"empty", "", ""},
		{"garbage", "not-an-ip", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeIPKey(tt.in); got != tt.want {
				t.Fatalf("normalizeIPKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeIPKey_SameAndDifferentPrefix is the property the issue calls out:
// two IPv6 addresses in the same /64 map to one key, while a different /64 maps
// to a different key — so a user's whole /64 shares a bucket but a neighbouring
// subnet is independent.
func TestNormalizeIPKey_SameAndDifferentPrefix(t *testing.T) {
	t.Parallel()

	a := normalizeIPKey("2001:db8:0:1::1")
	b := normalizeIPKey("2001:db8:0:1:ffff:ffff:ffff:ffff") // same /64 as a
	c := normalizeIPKey("2001:db8:0:2::1")                  // different /64

	if a == "" || a != b {
		t.Fatalf("addresses in the same /64 must share a key: a=%q b=%q", a, b)
	}
	if a == c {
		t.Fatalf("addresses in different /64s must not share a key: a=%q c=%q", a, c)
	}
}

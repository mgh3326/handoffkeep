package client

import "testing"

// Host normalization folds only what DNS folds; an IPv6 zone stays byte-exact.
func TestNormalizeHostKeepsZone(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"Example.COM", "example.com"},
		{"example.com.:8080", "example.com:8080"},
		{"10.0.0.1:443", "10.0.0.1:443"},
		{"[FE80::1]:444", "[fe80::1]:444"},
		{"[FE80::1]", "[fe80::1]"},
		{"[FE80::1%ZoneA]:444", "[fe80::1%ZoneA]:444"},
		{"[FE80::1%ZoneA]", "[fe80::1%ZoneA]"},
	} {
		if normalizeHost(tc.a) != normalizeHost(tc.b) {
			t.Errorf("normalizeHost(%q)=%q != normalizeHost(%q)=%q", tc.a, normalizeHost(tc.a), tc.b, normalizeHost(tc.b))
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"[fe80::1%ZoneA]:444", "[fe80::1%zonea]:444"},
		{"[fe80::1%ZoneA]:444", "[fe80::1%ZoneB]:444"},
		{"[fe80::1%ZoneA]", "[fe80::1%zonea]"},
		{"[fe80::1%ZoneA]:444", "[fe80::1]:444"},
		{"example.com:8080", "example.com:8081"},
	} {
		if normalizeHost(tc.a) == normalizeHost(tc.b) {
			t.Errorf("normalizeHost(%q) == normalizeHost(%q); want different origins", tc.a, tc.b)
		}
	}
}

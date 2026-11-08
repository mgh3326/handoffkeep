package hktransport

import (
	"errors"
	"net/http"
	"testing"
)

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

// The redirect guard judges every hop against the configured origin with the
// senders' comparison, not against the first request of the chain: a chain
// that somehow started off the origin still cannot hop on along it.
func TestRedirectGuardJudgesEachHopAgainstConfiguredOrigin(t *testing.T) {
	c := Client{URL: "http://hk.example:8080/base"}
	guard := c.httpClient().CheckRedirect
	req := func(u string) *http.Request {
		r, err := http.NewRequest("GET", u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	on := req("http://hk.example:8080/base/v1/a")
	for _, tc := range []struct {
		name string
		hop  string
		via  []*http.Request
		want error
	}{
		{"same origin", "http://HK.example.:8080/base/v1/b", []*http.Request{on}, nil},
		{"same origin, third hop", "http://hk.example:8080/c", []*http.Request{on, req("http://hk.example:8080/b")}, nil},
		{"chain started off origin", "http://collector.example/b", []*http.Request{req("http://collector.example/a")}, http.ErrUseLastResponse},
		{"foreign host mid-chain", "http://collector.example/c", []*http.Request{on, req("http://hk.example:8080/b")}, http.ErrUseLastResponse},
		{"unicode-folding host", "http://hK.example:8080/b", []*http.Request{on}, http.ErrUseLastResponse},
		{"other port", "http://hk.example/b", []*http.Request{on}, http.ErrUseLastResponse},
		{"scheme change", "https://hk.example:8080/b", []*http.Request{on}, http.ErrUseLastResponse},
		{"userinfo", "http://u@hk.example:8080/b", []*http.Request{on}, http.ErrUseLastResponse},
		{"ten hops", "http://hk.example:8080/b", []*http.Request{on, on, on, on, on, on, on, on, on, on}, ErrTooManyRedirects},
	} {
		if got := guard(req(tc.hop), tc.via); !errors.Is(got, tc.want) && got != tc.want {
			t.Errorf("%s: guard(%q) = %v, want %v", tc.name, tc.hop, got, tc.want)
		}
	}
}

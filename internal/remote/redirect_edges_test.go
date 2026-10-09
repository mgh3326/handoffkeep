package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// exactRouteTransport maps exact URL hosts (case and zone included) onto
// isolated httptest servers, so two origins that differ only in an IPv6 zone
// land on different listeners without DNS or a real interface. Unrouted hosts
// fail instead of dialing.
type exactRouteTransport struct {
	mu     sync.Mutex
	routes map[string]*httptest.Server
	sends  []recordedRequest
}

func (t *exactRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.sends = append(t.sends, recordedRequest{scheme: r.URL.Scheme, host: r.URL.Host, hdr: capture(r)})
	dest := t.routes[r.URL.Host]
	t.mu.Unlock()
	if dest == nil {
		return nil, errors.New("blocked_nonfixture_dial")
	}
	c := r.Clone(r.Context())
	u, _ := url.Parse(dest.URL)
	u.Path, u.RawPath, u.RawQuery = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
	c.URL, c.Host = u, u.Host
	resp, e := dest.Client().Transport.RoundTrip(c)
	if resp != nil {
		resp.Request = r
	}
	return resp, e
}

func (t *exactRouteTransport) snapshot() []recordedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]recordedRequest(nil), t.sends...)
}

// The zone of an IPv6 literal names an interface and is case-sensitive: a
// redirect that changes it, by case alone or to another zone, is a different
// origin and gets neither the CF pair nor the bearer. The identical zone is
// still the configured host and keeps following with credentials.
func TestIPv6ZoneChangeIsForeignOrigin(t *testing.T) {
	const base = "https://[fe80::1%25ZoneA]:444"
	for _, tc := range []struct {
		name, location string
		foreign        bool
	}{
		{"case-only", "https://[fe80::1%25zonea]:444/end", true},
		{"other-zone", "https://[fe80::1%25ZoneB]:444/end", true},
		{"same-zone", "https://[FE80::1%25ZoneA]:444/end", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var targetGot []capturedHeaders
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				targetGot = append(targetGot, capture(r))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{}")
			}))
			defer target.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/end" {
					mu.Lock()
					targetGot = append(targetGot, capture(r))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, "{}")
					return
				}
				w.Header().Set("Location", tc.location)
				w.WriteHeader(http.StatusFound)
			}))
			defer origin.Close()
			bu, _ := url.Parse(base)
			lu, _ := url.Parse(tc.location)
			rt := &exactRouteTransport{routes: map[string]*httptest.Server{bu.Host: origin}}
			if lu.Host != bu.Host {
				rt.routes[lu.Host] = target
			}
			c := Client{URL: base, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{Transport: rt}}
			_, _, e := c.GetDocument(context.Background(), "fixture")
			mu.Lock()
			got := append([]capturedHeaders(nil), targetGot...)
			mu.Unlock()
			if !tc.foreign {
				if e != nil || len(got) != 1 || got[0].id != cfFixtureID || got[0].secret != cfFixtureSecret || got[0].auth != "Bearer fixture-token" {
					t.Fatalf("same zone: err=%v hops=%d — the configured host must still follow with credentials", e, len(got))
				}
				return
			}
			var he *HTTPError
			if !errors.As(e, &he) || he.Code != "redirect_foreign_host" {
				t.Errorf("zone change: err=%v want redirect_foreign_host", e)
			}
			if len(got) != 0 || len(rt.snapshot()) != 1 {
				t.Fatalf("zone change: target received %d requests (sends=%d) — credentials crossed a zone change", len(got), len(rt.snapshot()))
			}
		})
	}
}

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

// errorLeaks reports which canaries appear anywhere in e: its text, %+v and
// %#v renderings and every error reachable through Unwrap, including a
// url.Error URL field.
func errorLeaks(e error, canaries ...string) []string {
	var found []string
	seen := map[string]bool{}
	var walk func(error, int)
	walk = func(e error, depth int) {
		if e == nil || depth > 32 {
			return
		}
		s := fmt.Sprintf("%v|%+v|%#v", e, e, e)
		if ue, ok := e.(*url.Error); ok {
			s += "|" + ue.URL
		}
		for _, c := range canaries {
			if strings.Contains(s, c) && !seen[c] {
				seen[c] = true
				found = append(found, c)
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				walk(x, depth+1)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap(), depth+1)
		}
	}
	walk(e, 0)
	return found
}

// wrappedPolicyError has safe text but wraps a cause carrying the refused URL.
type wrappedPolicyError struct{ cause error }

func (e wrappedPolicyError) Error() string { return "fixture_policy_refusal" }
func (e wrappedPolicyError) Unwrap() error { return e.cause }

// A caller-supplied redirect policy that refuses a hop may wrap the refused
// Location or quote the request URL; the returned error is the fixed
// redirect_refused and carries no byte of either, through Error, %+v, %#v or
// any Unwrap chain, on every request form that follows redirects.
func TestRefusedRedirectPolicyLeaksNothing(t *testing.T) {
	const marker = "location-canary.fixture"
	policies := map[string]func(*http.Request, []*http.Request) error{
		"wrapped-url-error": func(r *http.Request, _ []*http.Request) error {
			return wrappedPolicyError{&url.Error{Op: "fixture policy", URL: r.URL.String(), Err: errors.New("fixture refusal")}}
		},
		"text-with-url": func(r *http.Request, via []*http.Request) error {
			return fmt.Errorf("fixture policy refused %s after %s", r.URL.String(), via[0].URL.String())
		},
	}
	for pname, policy := range policies {
		for name := range allRequestForms(Client{}) {
			if name == "presign" {
				continue // presign never follows, so no policy runs
			}
			t.Run(pname+"/"+name, func(t *testing.T) {
				hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", "/"+marker+"?id="+cfFixtureID+"&secret="+cfFixtureSecret+"&bearer=fixture-token")
					w.WriteHeader(http.StatusFound)
				})
				c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{CheckRedirect: policy}}
				e := allRequestForms(c)[name](context.Background())
				var he *HTTPError
				if !errors.As(e, &he) || he.Code != "redirect_refused" {
					t.Errorf("err code mismatch: want redirect_refused (got %T)", e)
				}
				hu, _ := url.Parse(hk.URL)
				if leaks := errorLeaks(e, marker, cfFixtureID, cfFixtureSecret, "fixture-token", hu.Host, "/v1/", "fixture_policy_refusal", "fixture policy"); len(leaks) != 0 {
					t.Errorf("refused-redirect error leaks %d response-controlled or URL values", len(leaks))
				}
				if n := len(hk.requests()); n != 1 {
					t.Fatalf("server saw %d requests; the refused hop must never be sent", n)
				}
			})
		}
	}
}

// Ordinary dial, TLS and timeout failures on the configured URL keep Go's
// url.Error chain, text and timeout semantics; password userinfo in the URL
// changes nothing but the (redacted) URL, and the password never surfaces.
func TestConfiguredURLErrorsWithUserinfoKeepChain(t *testing.T) {
	const password = "userinfo-password.fixture"
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stall.Close()
	for _, mode := range []string{"dial", "tls", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			base := map[string]string{"dial": closedURL, "tls": tlsSrv.URL, "timeout": stall.URL}[mode]
			run := func(rawURL string) error {
				tr := &http.Transport{}
				defer tr.CloseIdleConnections()
				cl := &http.Client{Transport: tr}
				if mode == "timeout" {
					cl.Timeout = 100 * time.Millisecond
				}
				c := Client{URL: rawURL, Token: "fixture-token", HTTP: cl}
				_, _, e := c.GetDocument(context.Background(), "fixture")
				return e
			}
			u, _ := url.Parse(base)
			u.User = url.UserPassword("fixture-user", password)
			plain, withInfo := run(base), run(u.String())
			var pu, wu *url.Error
			if !errors.As(plain, &pu) {
				t.Fatalf("no-userinfo baseline is not a url.Error: %T", plain)
			}
			if !errors.As(withInfo, &wu) {
				t.Fatalf("userinfo error lost its url.Error chain: got %T %v", withInfo, withInfo)
			}
			if fmt.Sprintf("%T", pu.Err) != fmt.Sprintf("%T", wu.Err) {
				t.Errorf("cause type %T != baseline %T", wu.Err, pu.Err)
			}
			if pu.Timeout() != wu.Timeout() || errors.Is(plain, context.DeadlineExceeded) != errors.Is(withInfo, context.DeadlineExceeded) || errors.Is(plain, os.ErrDeadlineExceeded) != errors.Is(withInfo, os.ErrDeadlineExceeded) {
				t.Errorf("timeout semantics differ: baseline timeout=%t userinfo timeout=%t", pu.Timeout(), wu.Timeout())
			}
			if mode == "timeout" && !wu.Timeout() {
				t.Errorf("client timeout no longer reports Timeout()")
			}
			if a, b := strings.ReplaceAll(plain.Error(), pu.URL, "<url>"), strings.ReplaceAll(withInfo.Error(), wu.URL, "<url>"); a != b {
				t.Errorf("text modulo URL differs:\n userinfo: %s\n baseline: %s", b, a)
			}
			var he *HTTPError
			if errors.As(withInfo, &he) {
				t.Errorf("configured-URL failure classified as %s", he.Code)
			}
			if leaks := errorLeaks(withInfo, password); len(leaks) != 0 {
				t.Errorf("error output carries the userinfo password")
			}
		})
	}
}

// exactHopServer answers the first request with a 307 back to the exact
// request URI and fails every later request: "stall" blocks until the client
// gives up, "malformed" writes a response that is not HTTP.
func exactHopServer(t *testing.T, failure string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	release := make(chan struct{})
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if hits.Add(1) == 1 {
			w.Header().Set("Location", r.URL.RequestURI())
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if failure == "stall" {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		conn, buf, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error("fixture hijack failed")
			return
		}
		_, _ = buf.WriteString("invalid fixture response\r\n\r\n")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.Start()
	t.Cleanup(func() { close(release); s.Close() })
	return s, &hits
}

// A same-origin 307 back to the exact request URL names nothing but the
// configured URL, so a transport failure or timeout on that hop keeps Go's
// url.Error chain, Timeout() and transport text on every following request
// form, with or without password userinfo — as on main.
func TestExactURLHopFailureKeepsGoError(t *testing.T) {
	const password = "userinfo-password.fixture"
	for _, failure := range []string{"stall", "malformed"} {
		for _, userinfo := range []bool{false, true} {
			for name := range allRequestForms(Client{}) {
				if name == "presign" {
					continue // presign never follows
				}
				t.Run(fmt.Sprintf("%s/userinfo-%t/%s", failure, userinfo, name), func(t *testing.T) {
					s, hits := exactHopServer(t, failure)
					base := s.URL
					if userinfo {
						u, _ := url.Parse(base)
						u.User = url.UserPassword("fixture-user", password)
						base = u.String()
					}
					tr := &http.Transport{}
					defer tr.CloseIdleConnections()
					cl := &http.Client{Transport: tr}
					if failure == "stall" {
						cl.Timeout = 200 * time.Millisecond
					}
					c := Client{URL: base, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: cl}
					e := allRequestForms(c)[name](context.Background())
					var ue *url.Error
					var he *HTTPError
					if errors.As(e, &he) || !errors.As(e, &ue) {
						t.Fatalf("exact-URL hop failure lost Go's url.Error: got %T %v", e, e)
					}
					switch failure {
					case "stall":
						if !ue.Timeout() || !errors.Is(e, context.DeadlineExceeded) || !strings.Contains(e.Error(), "Client.Timeout exceeded") {
							t.Errorf("exact-URL hop timeout lost its timeout semantics: %v", e)
						}
					case "malformed":
						if !strings.Contains(e.Error(), "malformed HTTP") {
							t.Errorf("exact-URL hop transport text changed: %v", e)
						}
					}
					if hits.Load() != 2 {
						t.Errorf("server saw %d requests; want the 307 and the exact-URL hop", hits.Load())
					}
					if leaks := errorLeaks(e, password, cfFixtureID, cfFixtureSecret, "fixture-token"); len(leaks) != 0 {
						t.Errorf("error output carries %d credential values", len(leaks))
					}
				})
			}
		}
	}
}

// prefixTransport is a caller transport whose ordinary dial error text starts
// like Go's Location parse error, without any response or redirect.
type prefixTransport struct {
	next   http.RoundTripper
	format string
}

func (t prefixTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, e := t.next.RoundTrip(r)
	if e != nil {
		return nil, fmt.Errorf(t.format, e)
	}
	return resp, nil
}

// An error on the configured URL with no redirect response is never
// classified as a redirect, even when a caller transport's text shares Go's
// malformed-Location prefix: it equals a direct http.Client error, with or
// without password userinfo and with or without Client.Timeout.
func TestPrefixCollidingTransportKeepsChain(t *testing.T) {
	const password = "userinfo-password.fixture"
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	formats := map[string]string{
		"prefix-colon": "failed to parse Location header: upstream transport diagnostic: %w",
		"go-shaped":    `failed to parse Location header "/%%zz": parse "/%%zz": invalid URL escape "%%zz" (%w)`,
	}
	for fname, format := range formats {
		for _, timeout := range []time.Duration{0, 5 * time.Second} {
			for _, userinfo := range []bool{false, true} {
				if fname == "go-shaped" && timeout != 0 {
					continue // no recording transport under Client.Timeout; see do
				}
				t.Run(fmt.Sprintf("%s/timeout-%t/userinfo-%t", fname, timeout != 0, userinfo), func(t *testing.T) {
					base := closedURL
					if userinfo {
						u, _ := url.Parse(base)
						u.User = url.UserPassword("fixture-user", password)
						base = u.String()
					}
					tr := &http.Transport{}
					defer tr.CloseIdleConnections()
					cl := &http.Client{Transport: prefixTransport{tr, format}, Timeout: timeout}
					c := Client{URL: base, Token: "fixture-token", HTTP: cl}
					raw, _ := c.newRequest(context.Background(), "GET", "/v1/documents/fixture", nil)
					_, direct := cl.Do(raw)
					_, _, e := c.GetDocument(context.Background(), "fixture")
					var ue *url.Error
					var he *HTTPError
					if errors.As(e, &he) || !errors.As(e, &ue) {
						t.Fatalf("configured-URL error without a redirect was reclassified: got %T %v", e, e)
					}
					if direct == nil || e.Error() != direct.Error() {
						t.Errorf("text differs from a direct http.Client error:\n got: %v\nwant: %v", e, direct)
					}
					if leaks := errorLeaks(e, password); len(leaks) != 0 {
						t.Errorf("error output carries the userinfo password")
					}
				})
			}
		}
	}
}

// A real malformed Location is still redirect_location_invalid with no byte
// of it in the error: directly, after a hop back to the exact request URL,
// and with or without Client.Timeout.
func TestMalformedLocationStillClassified(t *testing.T) {
	const location = "https://team.cloudflareaccess.com/%zz?malformed-canary.fixture"
	for _, afterHop := range []bool{false, true} {
		for _, timeout := range []time.Duration{0, 5 * time.Second} {
			for name := range allRequestForms(Client{}) {
				if afterHop && name == "presign" {
					continue
				}
				t.Run(fmt.Sprintf("hop-%t/timeout-%t/%s", afterHop, timeout != 0, name), func(t *testing.T) {
					var hits atomic.Int32
					hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
						if hits.Add(1) == 1 && afterHop {
							w.Header().Set("Location", r.URL.RequestURI())
							w.WriteHeader(http.StatusTemporaryRedirect)
							return
						}
						w.Header().Set("Location", location)
						w.WriteHeader(http.StatusFound)
					})
					c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{Timeout: timeout}}
					e := allRequestForms(c)[name](context.Background())
					var he *HTTPError
					if !errors.As(e, &he) || he.Code != "redirect_location_invalid" {
						t.Fatalf("err=%v want redirect_location_invalid", e)
					}
					if leaks := errorLeaks(e, "malformed-canary", "%zz", cfFixtureID, cfFixtureSecret, "fixture-token"); len(leaks) != 0 {
						t.Errorf("malformed-Location error leaks %d values", len(leaks))
					}
				})
			}
		}
	}
}

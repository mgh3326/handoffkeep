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

// forgedParseErrors are caller-transport rewrites of a real dial error into
// text imitating Go's Location parse error, with no response behind it.
// "canonical" is byte-identical to Go's rendering for an unparseable
// Location; the other quotings name the same Location in forms fmt %q never
// produces.
func forgedParseErrors() map[string]func(error) error {
	parseErr := func(loc string) string { _, e := url.Parse(loc); return e.Error() }
	fixed := func(text string) func(error) error { return func(error) error { return errors.New(text) } }
	const prefix = "failed to parse Location header "
	return map[string]func(error) error{
		"prefix-colon": func(e error) error {
			return fmt.Errorf("failed to parse Location header: upstream transport diagnostic: %w", e)
		},
		"canonical":      fixed(fmt.Sprintf(prefix+"%q: %v", "/%zz", parseErr("/%zz"))),
		"hex-escape":     fixed(prefix + `"\x2f%zz": ` + parseErr("/%zz")),
		"unicode-escape": fixed(prefix + `"\u002f%zz": ` + parseErr("/%zz")),
		"raw-quoted":     fixed(prefix + "`/%zz`: " + parseErr("/%zz")),
		"rune-quoted":    fixed(prefix + `'%': ` + parseErr("%")),
		"ascii-escape":   fixed(fmt.Sprintf(prefix+"%+q: %v", "/\u00e9%zz", parseErr("/\u00e9%zz"))),
	}
}

// forgeTransport records each error it forges so the test can check the
// returned chain carries that very error, and the request it was asked to
// send so the test can replay it through a plain client.
type forgeTransport struct {
	next    http.RoundTripper
	rewrite func(error) error
	mu      sync.Mutex
	forged  []error
	last    *http.Request
}

func (t *forgeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.last = r.Clone(r.Context())
	t.mu.Unlock()
	resp, e := t.next.RoundTrip(r)
	if e != nil {
		e = t.rewrite(e)
		t.mu.Lock()
		t.forged = append(t.forged, e)
		t.mu.Unlock()
		return nil, e
	}
	return resp, nil
}

// directError sends the request the forging transport saw through a plain
// http.Client with the same transport and Timeout: the error net/http itself
// reports, whose text the production error must equal outside the excluded
// case.
func directError(t *testing.T, ft *forgeTransport, timeout time.Duration) error {
	t.Helper()
	ft.mu.Lock()
	last := ft.last
	ft.mu.Unlock()
	if last == nil {
		t.Fatal("forging transport was never invoked")
	}
	r, e := http.NewRequestWithContext(context.Background(), last.Method, last.URL.String(), nil)
	if e != nil {
		t.Fatalf("direct baseline request: %v", e)
	}
	r.Header = last.Header.Clone()
	_, de := (&http.Client{Transport: ft, Timeout: timeout}).Do(r)
	if de == nil {
		t.Fatal("direct baseline against a closed listener unexpectedly succeeded")
	}
	return de
}

// runForged sends one request form to a closed listener through a forging
// caller transport and returns the error, the error the transport forged and
// the text a direct http.Client reports for the same request.
func runForged(t *testing.T, form string, rewrite func(error) error, timeout time.Duration, userinfo bool) (error, error, error) {
	t.Helper()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := closed.URL
	closed.Close()
	if userinfo {
		u, _ := url.Parse(base)
		u.User = url.UserPassword("fixture-user", "userinfo-password.fixture")
		base = u.String()
	}
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	ft := &forgeTransport{next: tr, rewrite: rewrite}
	c := Client{URL: base, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{Transport: ft, Timeout: timeout}}
	e := allRequestForms(c)[form](context.Background())
	ft.mu.Lock()
	forged := ft.forged
	ft.mu.Unlock()
	if len(forged) != 1 {
		t.Fatalf("transport forged %d errors; want 1", len(forged))
	}
	return e, forged[0], directError(t, ft, timeout)
}

// assertPassedThrough checks an error on the configured URL kept Go's
// url.Error around the transport's own error — including Go's exact outer
// text, which must equal a direct http.Client error — as main returns an
// ordinary error, and carries no password or credential.
func assertPassedThrough(t *testing.T, e, forged, direct error) {
	t.Helper()
	var ue *url.Error
	var he *HTTPError
	if errors.As(e, &he) || !errors.As(e, &ue) {
		t.Fatalf("configured-URL error without a redirect was reclassified: got %T %v", e, e)
	}
	if ue.Err != forged {
		t.Errorf("url.Error no longer wraps the transport's own error: %v", e)
	}
	if e.Error() != direct.Error() {
		t.Errorf("text differs from a direct http.Client error:\n got: %v\nwant: %v", e, direct)
	}
	if leaks := errorLeaks(e, "userinfo-password.fixture", cfFixtureID, cfFixtureSecret, "fixture-token"); len(leaks) != 0 {
		t.Errorf("error output carries %d credential values", len(leaks))
	}
}

// Without a client deadline (Timeout zero or negative — Go ignores a
// nonpositive Timeout) the redirect response is recorded, so no forged
// parse-error text, canonical or not, turns a configured-URL error into a
// redirect: on every request form, with or without password userinfo.
func TestForgedParseErrorWithoutDeadlinePassesThrough(t *testing.T) {
	for fname, rewrite := range forgedParseErrors() {
		for _, timeout := range []time.Duration{0, -time.Second} {
			for _, userinfo := range []bool{false, true} {
				for form := range allRequestForms(Client{}) {
					t.Run(fmt.Sprintf("%s/timeout-%v/userinfo-%t/%s", fname, timeout, userinfo, form), func(t *testing.T) {
						e, forged, direct := runForged(t, form, rewrite, timeout, userinfo)
						assertPassedThrough(t, e, forged, direct)
					})
				}
			}
		}
	}
}

// Under a positive Client.Timeout only Go's canonical rendering is matched:
// a forged parse error with any other quoting, or without a quoted Location
// at all, passes through on every request form.
func TestNonCanonicalForgedParseErrorUnderTimeoutPassesThrough(t *testing.T) {
	for fname, rewrite := range forgedParseErrors() {
		if fname == "canonical" {
			continue
		}
		for _, userinfo := range []bool{false, true} {
			for form := range allRequestForms(Client{}) {
				t.Run(fmt.Sprintf("%s/userinfo-%t/%s", fname, userinfo, form), func(t *testing.T) {
					e, forged, direct := runForged(t, form, rewrite, 5*time.Second, userinfo)
					assertPassedThrough(t, e, forged, direct)
				})
			}
		}
	}
}

// Documented excluded case (invariant c amendment): a caller client with a
// positive Client.Timeout whose transport forges Go's canonical parse-error
// text is classified redirect_location_invalid — the fixed, leak-free error —
// because no redirect response can be recorded without changing timeout
// semantics. Pinned so any change to it is deliberate.
func TestCanonicalForgedParseErrorUnderTimeoutIsExcludedCase(t *testing.T) {
	rewrite := forgedParseErrors()["canonical"]
	for _, userinfo := range []bool{false, true} {
		for form := range allRequestForms(Client{}) {
			t.Run(fmt.Sprintf("userinfo-%t/%s", userinfo, form), func(t *testing.T) {
				e, _, _ := runForged(t, form, rewrite, 5*time.Second, userinfo)
				var he *HTTPError
				if !errors.As(e, &he) || he.Code != "redirect_location_invalid" {
					t.Fatalf("excluded case changed: err=%T want redirect_location_invalid", e)
				}
				if leaks := errorLeaks(e, "%zz", "userinfo-password.fixture", cfFixtureID, cfFixtureSecret, "fixture-token"); len(leaks) != 0 {
					t.Errorf("excluded-case error leaks %d values", len(leaks))
				}
			})
		}
	}
}

// A real malformed Location is still redirect_location_invalid with no byte
// of it in the error: directly, after a hop back to the exact request URL,
// and with Client.Timeout zero, negative or positive.
func TestMalformedLocationStillClassified(t *testing.T) {
	const location = "https://team.cloudflareaccess.com/%zz?malformed-canary.fixture"
	for _, afterHop := range []bool{false, true} {
		for _, timeout := range []time.Duration{0, -time.Second, 5 * time.Second} {
			for name := range allRequestForms(Client{}) {
				if afterHop && name == "presign" {
					continue
				}
				t.Run(fmt.Sprintf("hop-%t/timeout-%v/%s", afterHop, timeout, name), func(t *testing.T) {
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

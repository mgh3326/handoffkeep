package hktransport_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/hktransport"
)

const (
	fixtureToken    = "fixture-token"
	fixtureCFID     = "cf-access-id.fixture.invalid"
	fixtureCFSecret = "cf-access-secret-fixture"
)

// recorder is an httptest server that counts the requests it receives and
// keeps the credential headers each one carried.
type recorder struct {
	*httptest.Server
	mu    sync.Mutex
	hits  int
	creds []string
}

func newRecorder(t *testing.T, h http.HandlerFunc) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits++
		r.creds = append(r.creds, req.Header.Get("Authorization")+"|"+req.Header.Get("CF-Access-Client-Id")+"|"+req.Header.Get("CF-Access-Client-Secret"))
		r.mu.Unlock()
		if h != nil {
			h(w, req)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) count() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, append([]string(nil), r.creds...)
}

func hkClient(u string) hktransport.Client {
	return hktransport.Client{URL: u, Token: fixtureToken, CFAccessClientID: fixtureCFID, CFAccessClientSecret: fixtureCFSecret}
}

func assertUntouched(t *testing.T, name string, r *recorder) {
	t.Helper()
	if n, creds := r.count(); n != 0 {
		t.Fatalf("%s received %d requests (credential headers %q); a credential-bearing request must never leave the configured hk origin", name, n, creds)
	}
}

// A request path is joined onto the configured URL, so a path that is not a
// single-slash absolute path can move the request — and the bearer token and
// CF Access pair it carries — to another host. The builder refuses it and
// returns no request; Call never sends one.
func TestNewRequestRefusesPathsThatLeaveTheOrigin(t *testing.T) {
	collector := newRecorder(t, nil)
	hk := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	collectorHost := strings.TrimPrefix(collector.URL, "http://")
	c := hkClient(hk.URL)
	for _, path := range []string{
		"@collector.example/",
		"@" + collectorHost + "/",
		"//collector.example/x",
		"//" + collectorHost + "/x",
		"http://collector.example/x",
		"http://" + collectorHost + "/x",
		"x",
		"",
	} {
		r, err := c.NewRequest(context.Background(), "GET", path, nil)
		if err == nil || r != nil {
			got := "<nil>"
			if r != nil {
				got = r.URL.String()
			}
			t.Errorf("NewRequest(path=%q) = %s, %v; want an error and no request", path, got, err)
		}
		if err := c.Call(context.Background(), "GET", path, nil, nil); err == nil {
			t.Errorf("Call(path=%q) succeeded; want a refusal", path)
		}
	}
	assertUntouched(t, "collector", collector)
	assertUntouched(t, "hk", hk)

	// A rooted path is still a normal hk call carrying the credentials.
	if err := c.Call(context.Background(), "GET", "/v1/tasks/1", nil, nil); err != nil {
		t.Fatalf("Call(/v1/tasks/1): %v", err)
	}
	if n, creds := hk.count(); n != 1 || creds[0] != "Bearer "+fixtureToken+"|"+fixtureCFID+"|"+fixtureCFSecret {
		t.Fatalf("hk hits=%d creds=%q", n, creds)
	}
}

// The configured URL must be an absolute http or https URL with a host; any
// other value builds no request. A path prefix on the URL is kept, as the
// join always kept it.
func TestNewRequestRequiresAbsoluteHTTPURL(t *testing.T) {
	for _, u := range []string{"", "hk.example", "/v1", "ftp://hk.example", "http://", "https:///v1", "mailto:ops@hk.example"} {
		if r, err := hkClient(u).NewRequest(context.Background(), "GET", "/v1/tasks/1", nil); err == nil || r != nil {
			t.Errorf("NewRequest with URL %q succeeded; want a refusal", u)
		}
	}
	var mu sync.Mutex
	var gotPath string
	hk := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.RequestURI()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	for _, base := range []string{hk.URL + "/hk", hk.URL + "/hk/", strings.Replace(hk.URL, "http://", "HTTP://", 1) + "/hk"} {
		if err := hkClient(base).Call(context.Background(), "GET", "/v1/tasks/1?x=1", nil, nil); err != nil {
			t.Fatalf("Call with URL %q: %v", base, err)
		}
		mu.Lock()
		got := gotPath
		mu.Unlock()
		if got != "/hk/v1/tasks/1?x=1" {
			t.Fatalf("URL %q: request URI %q, want the path prefix kept", base, got)
		}
	}
}

// A request built for the configured origin and then pointed somewhere else
// is refused by the sender before any network I/O, on the following and the
// no-follow path alike, so the credentials it carries never leave hk.
func TestSenderRefusesRetargetedRequest(t *testing.T) {
	collector := newRecorder(t, nil)
	hk := newRecorder(t, nil)
	c := hkClient(hk.URL)
	hkHost := strings.TrimPrefix(hk.URL, "http://")
	for _, target := range []string{
		collector.URL + "/x",
		"https://" + hkHost + "/x", // same host, other scheme
		"http://collector.example/x",
	} {
		for name, send := range map[string]func(*http.Request) (*http.Response, error){
			"Do":         c.Do,
			"DoNoFollow": c.DoNoFollow,
		} {
			r, err := c.NewRequest(context.Background(), "GET", "/v1/tasks/1", nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			r.URL, r.Host = u, u.Host
			resp, err := send(r)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, hktransport.ErrForeignOrigin) {
				t.Errorf("%s to %s: err=%v, want ErrForeignOrigin", name, target, err)
			}
		}
	}
	assertUntouched(t, "collector", collector)
	assertUntouched(t, "hk", hk)
}

// Go parses a redirect's Location before any redirect policy runs and puts it
// verbatim in the error when it does not parse. The no-follow sender (the
// presign and wrk path) and the following sender both collapse that error to
// a named one, so a signed query in the Location never reaches a log.
func TestMalformedLocationNeverInError(t *testing.T) {
	const canary = "SECRET-canary-3544"
	hk := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/%zz?sig="+canary)
		w.WriteHeader(http.StatusFound)
	})
	for _, timeout := range []time.Duration{0, time.Minute} {
		c := hkClient(hk.URL)
		c.HTTP = &http.Client{Timeout: timeout}
		for name, send := range map[string]func(*http.Request) (*http.Response, error){
			"Do":         c.Do,
			"DoNoFollow": c.DoNoFollow,
		} {
			r, err := c.NewRequest(context.Background(), "GET", "/v1/attachments/ab?presign=1", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := send(r)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("%s (timeout %v): no error for an unparseable Location", name, timeout)
			}
			for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(s, canary) {
					t.Fatalf("%s (timeout %v): error carries the Location: %s", name, timeout, s)
				}
			}
			if hktransport.ErrorCode(err) != "redirect_location_invalid" {
				t.Fatalf("%s (timeout %v): err=%v, want redirect_location_invalid", name, timeout, err)
			}
		}
	}
}

// stubTransport answers the first request with a redirect to loc and any
// later one with 200 {}, recording every request it sees, so a test observes
// the redirect guard's decision without resolving DNS.
type stubTransport struct {
	loc  string
	mu   sync.Mutex
	seen []string // host|Authorization|CF-Access-Client-Id|CF-Access-Client-Secret
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	first := len(s.seen) == 0
	s.seen = append(s.seen, r.URL.Host+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("CF-Access-Client-Id")+"|"+r.Header.Get("CF-Access-Client-Secret"))
	s.mu.Unlock()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}
	if first && s.loc != "" {
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", s.loc)
	}
	return resp, nil
}

func (s *stubTransport) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// unicodeFoldingHosts pairs a configured ASCII host with a non-ASCII host
// that Unicode lowercasing folds onto it (İ U+0130 -> i, K U+212A -> k) but
// that Go's transport sends, after IDNA mapping, to a different DNS name.
var unicodeFoldingHosts = []struct{ configured, foreign string }{
	{"i.example", "İ.example"},
	{"k.example", "K.example"},
	{"hk.example", "hK.example"},
}

// A redirect to a host that only Unicode case folding makes equal to the
// configured one is a different destination; the guard refuses it, so no
// request — and no CF Access pair — goes there.
func TestRedirectToUnicodeFoldingHostRefused(t *testing.T) {
	for _, h := range unicodeFoldingHosts {
		for _, loc := range []string{
			"http://" + h.foreign + "/capture",
			"//" + h.foreign + "/capture",
			"http://" + url.PathEscape(h.foreign) + "/capture",
		} {
			for name, call := range map[string]func(hktransport.Client) error{
				"Do": func(c hktransport.Client) error {
					r, err := c.NewRequest(context.Background(), "GET", "/v1/tasks/1", nil)
					if err != nil {
						return err
					}
					resp, err := c.Do(r)
					if resp != nil {
						resp.Body.Close()
					}
					return err
				},
				"Call": func(c hktransport.Client) error {
					err := c.Call(context.Background(), "GET", "/v1/tasks/1", nil, nil)
					if hktransport.ErrorCode(err) != "redirect_foreign_host" {
						return fmt.Errorf("Call err=%v, want redirect_foreign_host", err)
					}
					return nil
				},
			} {
				stub := &stubTransport{loc: loc}
				c := hkClient("http://" + h.configured)
				c.HTTP = &http.Client{Transport: stub}
				if err := call(c); err != nil {
					t.Errorf("%s with redirect to %q: %v", name, loc, err)
				}
				if seen := stub.requests(); len(seen) != 1 {
					t.Errorf("%s from http://%s followed a redirect to %q: requests %q; want only the first", name, h.configured, loc, seen)
				}
			}
		}
	}
}

// A request retargeted to a Unicode-folding host is refused by both senders
// before any I/O.
func TestSenderRefusesUnicodeFoldingHost(t *testing.T) {
	for _, h := range unicodeFoldingHosts {
		for name, send := range map[string]func(hktransport.Client, *http.Request) (*http.Response, error){
			"Do":         hktransport.Client.Do,
			"DoNoFollow": hktransport.Client.DoNoFollow,
		} {
			stub := &stubTransport{}
			c := hkClient("http://" + h.configured)
			c.HTTP = &http.Client{Transport: stub}
			r, err := c.NewRequest(context.Background(), "GET", "/v1/tasks/1", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.URL.Host, r.Host = h.foreign, h.foreign
			resp, err := send(c, r)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, hktransport.ErrForeignOrigin) {
				t.Errorf("%s to %q (configured %q): err=%v, want ErrForeignOrigin", name, h.foreign, h.configured, err)
			}
			if seen := stub.requests(); len(seen) != 0 {
				t.Errorf("%s to %q sent %q", name, h.foreign, seen)
			}
		}
	}
}

// newForwardProxy is a plain HTTP forward proxy: it sends each request to the
// absolute target it was asked for, headers included.
func newForwardProxy(t *testing.T) *httptest.Server {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		out, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = req.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.Close)
	return p
}

// A request whose URL keeps the configured scheme and host but carries an
// opaque target, userinfo, or a Host header naming another host can still
// leave the origin — through a forward proxy the opaque value or the Host
// header becomes the absolute request target. Both senders refuse it before
// any I/O, with or without a proxy.
func TestSenderRefusesOpaqueUserinfoAndHostOverride(t *testing.T) {
	collector := newRecorder(t, nil)
	hk := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	proxy := newForwardProxy(t)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	collectorHost := strings.TrimPrefix(collector.URL, "http://")
	viaProxy := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"opaque", func(r *http.Request) { r.URL.Opaque = "//" + collectorHost + "/capture" }},
		{"userinfo", func(r *http.Request) { r.URL.User = url.UserPassword("u", "p") }},
		{"host header", func(r *http.Request) { r.Host = collectorHost }},
	} {
		for _, hc := range []*http.Client{nil, viaProxy} {
			for name, send := range map[string]func(hktransport.Client, *http.Request) (*http.Response, error){
				"Do":         hktransport.Client.Do,
				"DoNoFollow": hktransport.Client.DoNoFollow,
			} {
				c := hkClient(hk.URL)
				c.HTTP = hc
				r, err := c.NewRequest(context.Background(), "GET", "/v1/tasks/1", nil)
				if err != nil {
					t.Fatal(err)
				}
				tc.mutate(r)
				resp, err := send(c, r)
				if resp != nil {
					resp.Body.Close()
				}
				if !errors.Is(err, hktransport.ErrForeignOrigin) {
					t.Errorf("%s with %s (proxy %v): err=%v, want ErrForeignOrigin", name, tc.name, hc != nil, err)
				}
			}
		}
	}
	assertUntouched(t, "collector", collector)
	assertUntouched(t, "hk", hk)

	// The proxy harness does reach hk for an unmodified request, so the
	// zero counts above are the senders' refusals, not a broken proxy.
	c := hkClient(hk.URL)
	c.HTTP = viaProxy
	if err := c.Call(context.Background(), "GET", "/v1/tasks/1", nil, nil); err != nil {
		t.Fatalf("Call through the proxy: %v", err)
	}
	if n, _ := hk.count(); n != 1 {
		t.Fatalf("hk hits through the proxy = %d, want 1", n)
	}
	assertUntouched(t, "collector", collector)
}

// The configured URL must name its host in ASCII — punycode for an
// internationalized name.
func TestConfiguredURLRefusesNonASCIIHost(t *testing.T) {
	for _, u := range []string{
		"http://\u0130.example",
		"http://%C4%B0.example",
		"https://\u212A.example/hk",
		"http://h\u00e9.example:8080",
	} {
		if r, err := hkClient(u).NewRequest(context.Background(), "GET", "/v1/tasks/1", nil); !errors.Is(err, hktransport.ErrInvalidURL) || r != nil {
			t.Errorf("NewRequest with URL %q: err=%v; want ErrInvalidURL and no request", u, err)
		}
	}
}

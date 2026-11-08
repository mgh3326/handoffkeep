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

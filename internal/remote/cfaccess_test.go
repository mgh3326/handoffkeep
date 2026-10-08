package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Fixture values are deliberately synthetic; they must never reach output.
const (
	cfFixtureID     = "cf-access-id.fixture.invalid"
	cfFixtureSecret = "cf-access-secret.fixture.invalid"
)

// capturedHeaders records the auth surface of one request the fake hk server
// received.
type capturedHeaders struct {
	id, secret, ua, auth string
	cfKeys               bool // any CF-Access-* key present at all
}

func capture(r *http.Request) capturedHeaders {
	_, hasID := r.Header["Cf-Access-Client-Id"]
	_, hasSecret := r.Header["Cf-Access-Client-Secret"]
	return capturedHeaders{
		id:     r.Header.Get("CF-Access-Client-Id"),
		secret: r.Header.Get("CF-Access-Client-Secret"),
		ua:     r.Header.Get("User-Agent"),
		auth:   r.Header.Get("Authorization"),
		cfKeys: hasID || hasSecret,
	}
}

// requestRecorder wraps an httptest server that records the header surface of
// every request and serves the routes the CF Access tests exercise.
type requestRecorder struct {
	*httptest.Server
	mu  sync.Mutex
	got []capturedHeaders
}

func newRequestRecorder(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *requestRecorder {
	t.Helper()
	rec := &requestRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.got = append(rec.got, capture(r))
		rec.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *requestRecorder) requests() []capturedHeaders {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedHeaders(nil), r.got...)
}

// serveHK answers every route the remote client can hit: a call() JSON GET, a
// call() JSON POST, the attachment PUT, presign, GET, and the tasks export.
func serveHK(t *testing.T, storage *httptest.Server) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/projects":
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []string{"p"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkpoints":
			_ = json.NewEncoder(w).Encode(store.Checkpoint{Session: "s"})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/attachments":
			_ = json.NewEncoder(w).Encode(map[string]any{"attachment": store.Attachment{SHA256: "ab"}, "created": true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/attachments/ab" && r.URL.Query().Get("presign") == "1":
			w.Header().Set("Location", storage.URL+"/obj")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/attachments/ab":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "bytes")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/export":
			_, _ = io.WriteString(w, `{"tasks":[]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

// exerciseAll runs one request of every kind the client can send.
func exerciseAll(t *testing.T, c Client, storage *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	if _, e := c.ListTaskProjects(ctx); e != nil {
		t.Fatalf("call GET: %v", e)
	}
	if _, e := c.Checkpoint(ctx, "", store.Checkpoint{Session: "s"}); e != nil {
		t.Fatalf("call POST: %v", e)
	}
	if _, _, e := c.PutAttachment(ctx, "", "n.bin", "application/octet-stream", "ref:x", []byte("b")); e != nil {
		t.Fatalf("PutAttachment: %v", e)
	}
	u, e := c.AttachmentURL(ctx, "ab")
	if e != nil {
		t.Fatalf("AttachmentURL: %v", e)
	}
	if u != storage.URL+"/obj" {
		t.Fatalf("presign location=%q want %q", u, storage.URL+"/obj")
	}
	att, body, e := c.GetAttachment(ctx, "ab")
	if e != nil {
		t.Fatalf("GetAttachment: %v", e)
	}
	_ = body.Close()
	if att.MIME != "application/octet-stream" {
		t.Fatalf("attachment mime=%q", att.MIME)
	}
	if _, e := c.ExportTasks(ctx, "", "", "", nil, 0); e != nil {
		t.Fatalf("ExportTasks: %v", e)
	}
}

// Every request kind carries the CF Access pair by equality plus the explicit
// User-Agent when the client is configured, and the legitimate presign 302 to
// the storage host still surfaces its Location.
func TestCFAccessHeadersOnEveryRequestKind(t *testing.T) {
	var storageHits atomic.Int32
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageHits.Add(1)
	}))
	defer storage.Close()
	hk := newRequestRecorder(t, serveHK(t, storage))
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}

	exerciseAll(t, c, storage)

	got := hk.requests()
	if len(got) != 6 {
		t.Fatalf("requests=%d want 6", len(got))
	}
	for i, h := range got {
		if h.id != cfFixtureID {
			t.Fatalf("request %d CF-Access-Client-Id=%q want %q", i, h.id, cfFixtureID)
		}
		if h.secret != cfFixtureSecret {
			t.Fatalf("request %d CF-Access-Client-Secret=%q want %q", i, h.secret, cfFixtureSecret)
		}
		if h.ua != UserAgent {
			t.Fatalf("request %d User-Agent=%q want %q", i, h.ua, UserAgent)
		}
		if h.auth != "Bearer fixture-token" {
			t.Fatalf("request %d Authorization=%q", i, h.auth)
		}
	}
	if storageHits.Load() != 0 {
		t.Fatalf("storage host received %d requests; the presign URL is returned, never followed", storageHits.Load())
	}
}

// Without the pair configured no CF-Access header key exists on any request;
// only the explicit User-Agent is added.
func TestNoCFHeadersWhenUnconfigured(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("storage host must never receive a request")
	}))
	defer storage.Close()
	hk := newRequestRecorder(t, serveHK(t, storage))
	c := Client{URL: hk.URL, Token: "fixture-token"}

	exerciseAll(t, c, storage)

	for i, h := range hk.requests() {
		if h.cfKeys {
			t.Fatalf("request %d carried CF headers with no keys configured", i)
		}
		if h.ua != UserAgent {
			t.Fatalf("request %d User-Agent=%q want %q", i, h.ua, UserAgent)
		}
	}
}

// A half-configured pair is a named config error naming the missing key and
// never printing a value.
func TestCFAccessPairErrorNamesMissingKey(t *testing.T) {
	err := CFAccessPairError(cfFixtureID, "")
	if err == nil || !strings.Contains(err.Error(), "cf_access_config_incomplete") {
		t.Fatalf("err=%v want cf_access_config_incomplete", err)
	}
	if !strings.Contains(err.Error(), "HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET") {
		t.Fatalf("err=%v must name the missing key", err)
	}
	if strings.Contains(err.Error(), cfFixtureID) {
		t.Fatalf("err=%v leaks the configured value", err)
	}
	err = CFAccessPairError("", cfFixtureSecret)
	if err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_CF_ACCESS_CLIENT_ID") {
		t.Fatalf("err=%v must name HANDOFFKEEP_CF_ACCESS_CLIENT_ID", err)
	}
	if strings.Contains(err.Error(), cfFixtureSecret) {
		t.Fatalf("err=%v leaks the configured value", err)
	}
	if err := CFAccessPairError("", ""); err != nil {
		t.Fatalf("absent pair err=%v", err)
	}
	if err := CFAccessPairError(cfFixtureID, cfFixtureSecret); err != nil {
		t.Fatalf("complete pair err=%v", err)
	}
}

// A redirect to a Cloudflare Access login is the named cf_access_login_redirect
// error on every request kind and is never followed — the error, not a dial,
// is what comes back.
func TestCFAccessLoginRedirectFailsLoud(t *testing.T) {
	const loginURL = "https://team-one.cloudflareaccess.com/cdn-cgi/access/login?redirect_url=signed"
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loginURL)
		w.WriteHeader(http.StatusFound)
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ctx := context.Background()

	check := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "cf_access_login_redirect") {
			t.Fatalf("%s: err=%v want cf_access_login_redirect", name, err)
		}
		for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token", loginURL, "redirect_url"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: error leaks %q: %v", name, leak, err)
			}
		}
	}
	_, e := c.ListTaskProjects(ctx)
	check("call", e)
	_, _, e = c.PutAttachment(ctx, "", "n", "text/plain", "", nil)
	check("PutAttachment", e)
	_, e = c.AttachmentURL(ctx, "ab")
	check("AttachmentURL", e)
	_, rc, e := c.GetAttachment(ctx, "ab")
	check("GetAttachment", e)
	if rc != nil {
		_ = rc.Close()
	}
	_, e = c.ExportTasks(ctx, "", "", "", nil, 0)
	check("ExportTasks", e)
}

// A redirect off the configured host is never followed — the foreign host
// receives no request at all, so no header can reach it.
func TestCrossHostRedirectNeverFollowed(t *testing.T) {
	var foreignHits atomic.Int32
	var foreignHeaders []capturedHeaders
	var fmu sync.Mutex
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		fmu.Lock()
		foreignHeaders = append(foreignHeaders, capture(r))
		fmu.Unlock()
	}))
	defer foreign.Close()
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", foreign.URL+"/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ctx := context.Background()

	if _, e := c.ListTaskProjects(ctx); e == nil || !strings.Contains(e.Error(), "redirect_foreign_host") {
		t.Fatalf("call: err=%v want redirect_foreign_host", e)
	}
	if _, rc, e := c.GetAttachment(ctx, "ab"); e == nil || !strings.Contains(e.Error(), "redirect_foreign_host") {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatalf("GetAttachment: err=%v want redirect_foreign_host", e)
	}
	if foreignHits.Load() != 0 {
		t.Fatalf("foreign host received %d requests", foreignHits.Load())
	}
	for _, h := range foreignHeaders {
		if h.cfKeys {
			t.Fatal("CF headers reached a foreign host")
		}
	}
}

// A redirect that stays on the configured host is still followed exactly as
// before — only cross-host hops are refused. The Location here is an
// absolute http URL on the same host:port, the shape tailnet deployments
// actually serve, so http -> http must keep working unchanged.
func TestSameHostRedirectStillFollowed(t *testing.T) {
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/tasks/projects" {
			w.Header().Set("Location", "http://"+r.Host+"/v1/tasks/projects2")
			w.WriteHeader(http.StatusFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []string{"p"}})
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ps, e := c.ListTaskProjects(context.Background())
	if e != nil {
		t.Fatalf("same-host redirect must still be followed: %v", e)
	}
	if len(ps) != 1 || ps[0] != "p" {
		t.Fatalf("projects=%v", ps)
	}
	got := hk.requests()
	if len(got) != 2 {
		t.Fatalf("requests=%d want 2", len(got))
	}
	for i, h := range got {
		if h.id != cfFixtureID || h.secret != cfFixtureSecret {
			t.Fatalf("hop %d lost the CF headers on a same-host redirect", i)
		}
		if h.auth != "Bearer fixture-token" {
			t.Fatalf("hop %d lost Authorization on a same-host redirect", i)
		}
	}
}

// recordTransport captures the scheme and auth surface of every request the
// underlying RoundTripper is asked to send — proof of whether a refused
// redirect hop was ever attempted at all.
type recordTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	seen []recordedRequest
}

type recordedRequest struct {
	scheme string
	hdr    capturedHeaders
}

func (t *recordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.seen = append(t.seen, recordedRequest{scheme: r.URL.Scheme, hdr: capture(r)})
	t.mu.Unlock()
	return t.next.RoundTrip(r)
}

// An https hk answering a same-host redirect to a plaintext Location must be
// refused: Go would resend Authorization and the CF pair on the follow-up
// hop, so no request may go out over http at all. The surfaced error is the
// named redirect_scheme_downgrade and never carries the Location value.
func TestHTTPSDowngradeRedirectRefused(t *testing.T) {
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+r.Host+"/downgraded?leak=1")
		w.WriteHeader(http.StatusFound)
	}))
	defer https.Close()
	rt := &recordTransport{next: https.Client().Transport}
	c := Client{URL: https.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret, HTTP: &http.Client{Transport: rt}}
	ctx := context.Background()

	_, e := c.ListTaskProjects(ctx)
	if e == nil || !strings.Contains(e.Error(), "redirect_scheme_downgrade") {
		t.Fatalf("call: err=%v want redirect_scheme_downgrade", e)
	}
	for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token", "downgraded", "leak=1", "http://"} {
		if strings.Contains(e.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, e)
		}
	}
	// The presign 302 path classifies the same downgrade rather than handing
	// the caller a plaintext URL.
	if _, e := c.AttachmentURL(ctx, "ab"); e == nil || !strings.Contains(e.Error(), "redirect_scheme_downgrade") {
		t.Fatalf("AttachmentURL: err=%v want redirect_scheme_downgrade", e)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i, s := range rt.seen {
		if s.scheme != "https" {
			t.Fatalf("request %d was sent over %q — the downgrade hop must never leave", i, s.scheme)
		}
	}
	if len(rt.seen) != 2 {
		t.Fatalf("transport saw %d requests; want the 2 refused redirect hops unsent", len(rt.seen))
	}
}

// A same-host redirect loop ends with an error after Go's default ten hops
// rather than hanging on a generous context — the wrapper must not strip the
// limit when the wrapped client has no policy of its own.
func TestSameHostRedirectLoopStopsAfterTen(t *testing.T) {
	var hits atomic.Int32
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", r.URL.RequestURI())
		w.WriteHeader(http.StatusFound)
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := c.ListTaskProjects(ctx)
	if e == nil || !strings.Contains(e.Error(), "stopped after 10 redirects") {
		t.Fatalf("err=%v want stopped after 10 redirects", e)
	}
	if hits.Load() != 10 {
		t.Fatalf("hops=%d want 10", hits.Load())
	}
}

// A 200 text/html answer on a JSON API call is a failure, never a success:
// that shape is the Access login page served in place of the API.
func TestHTML200IsErrorOnJSONCalls(t *testing.T) {
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body>login</body></html>")
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ctx := context.Background()

	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: 200 text/html must not be a success", name)
		}
		if !strings.Contains(err.Error(), "unexpected_html_response") {
			t.Fatalf("%s: err=%v want unexpected_html_response", name, err)
		}
		for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: error leaks %q: %v", name, leak, err)
			}
		}
	}
	_, e := c.ListTaskProjects(ctx)
	check("call", e)
	_, _, e = c.PutAttachment(ctx, "", "n", "text/plain", "", nil)
	check("PutAttachment", e)
	_, e = c.ExportTasks(ctx, "", "", "", nil, 0)
	check("ExportTasks", e)
}

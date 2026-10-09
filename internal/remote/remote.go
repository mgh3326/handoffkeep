// Package remote adapts the authenticated HTTP API for local CLI and stdio MCP.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// UserAgent is stamped on every hk request: Cloudflare Access can be
// configured to block the default Go user agent before a request reaches the
// service, and a distinct UA keeps hk calls identifiable in Access logs.
var UserAgent = "handoffkeep/" + buildRevision()

func buildRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}

type Client struct {
	URL, Token string
	// CFAccessClientID and CFAccessClientSecret carry the optional Cloudflare
	// Access service token for an hk URL behind Access. They are attached only
	// as a pair and only to requests against the configured URL; a redirect to
	// a different host is never followed, so they cannot leak off it.
	CFAccessClientID     string
	CFAccessClientSecret string
	HTTP                 *http.Client
}

// CFAccessPairError rejects a half-configured Cloudflare Access service
// token: one key without the other is a config error that names the missing
// key and never prints a value.
func CFAccessPairError(id, secret string) error {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	switch {
	case id != "" && secret == "":
		return errors.New("cf_access_config_incomplete: HANDOFFKEEP_CF_ACCESS_CLIENT_ID is set but HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET is missing")
	case id == "" && secret != "":
		return errors.New("cf_access_config_incomplete: HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET is set but HANDOFFKEEP_CF_ACCESS_CLIENT_ID is missing")
	}
	return nil
}

// cfAccessConfigured reports whether the service-token pair is present;
// whitespace-only values count as unset.
func (c Client) cfAccessConfigured() bool {
	return strings.TrimSpace(c.CFAccessClientID) != "" && strings.TrimSpace(c.CFAccessClientSecret) != ""
}

// newRequest builds one request against the configured hk URL carrying every
// header an hk call sends: bearer auth, the explicit User-Agent, and — only
// when both are configured — the Cloudflare Access service-token pair.
func (c Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("User-Agent", UserAgent)
	if id, secret := strings.TrimSpace(c.CFAccessClientID), strings.TrimSpace(c.CFAccessClientSecret); id != "" && secret != "" {
		r.Header.Set("CF-Access-Client-Id", id)
		r.Header.Set("CF-Access-Client-Secret", secret)
	}
	return r, nil
}

// normalizeHost folds a URL Host (hostname[:port]) to the form DNS treats as
// equal: lowercase with a trailing FQDN dot removed.
func normalizeHost(h string) string {
	if host, port, e := net.SplitHostPort(h); e == nil {
		return foldHostname(host) + ":" + port
	}
	return foldHostname(h)
}

// foldHostname folds one hostname. The zone of an IPv6 literal
// (fe80::1%zone) names an interface, which Go resolves case-sensitively, so
// only the address part is folded and the zone is kept byte-exact — a zone
// that differs, even by case alone, is a different origin.
func foldHostname(h string) string {
	if strings.Contains(h, ":") {
		if i := strings.IndexByte(h, '%'); i >= 0 {
			return strings.ToLower(strings.TrimSuffix(h[:i], ".")) + h[i:]
		}
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// httpClient returns the client for one hk request wrapped so a redirect that
// would move the Authorization or CF Access headers outside the configured
// host or scheme is never followed — Go re-sends custom headers on redirects.
// An https -> plaintext hop is refused for the same reason as a cross-host
// hop, as is an http -> https upgrade: credentials are scoped to the
// configured scheme, not just the host. When the wrapped client has no policy
// of its own, Go's default 10-redirect cap is preserved so a same-host loop
// cannot hang a call on a generous context.
func (c Client) httpClient() *http.Client {
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	hc := *h
	inner := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			switch {
			case normalizeHost(req.URL.Host) != normalizeHost(via[0].URL.Host):
				return http.ErrUseLastResponse
			case !strings.EqualFold(req.URL.Scheme, via[0].URL.Scheme):
				return http.ErrUseLastResponse
			}
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= 10 {
			return ErrTooManyRedirects
		}
		return nil
	}
	return &hc
}

// ErrTooManyRedirects is the wrapper's own redirect cap: a same-host loop
// stops after Go's default ten hops. It is the only redirect-policy error
// returned as is; callers recognize it with errors.Is.
var ErrTooManyRedirects = errors.New("stopped after 10 redirects")

// httpClientNoFollow never follows a redirect: the presign answer must surface
// its Location to the caller rather than forward the CF Access headers — or
// any headers — to the storage host.
func (c Client) httpClientNoFollow() *http.Client {
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	hc := *h
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// isRedirectStatus reports the 3xx statuses whose Location a client may follow.
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// cfAccessLoginHost reports whether host is a Cloudflare Access login host
// (<team>.cloudflareaccess.com or the apex). DNS names compare
// case-insensitively with an optional trailing dot.
func cfAccessLoginHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "cloudflareaccess.com" || strings.HasSuffix(host, ".cloudflareaccess.com")
}

// sameOriginHost reports whether h names the configured hk host — host
// comparisons fold case and a trailing dot and include the port.
func (c Client) sameOriginHost(h string) bool {
	base, e := url.Parse(strings.TrimRight(c.URL, "/"))
	return e == nil && normalizeHost(h) == normalizeHost(base.Host)
}

// sameHostSchemeChange reports whether u stays on the configured hk host but
// switches scheme — a redirect in either direction (upgrade or downgrade)
// moves the request outside the scheme the credentials were configured for.
func (c Client) sameHostSchemeChange(u *url.URL) bool {
	if !u.IsAbs() || !c.sameOriginHost(u.Host) {
		return false
	}
	base, e := url.Parse(strings.TrimRight(c.URL, "/"))
	return e == nil && !strings.EqualFold(u.Scheme, base.Scheme)
}

// redirectError classifies a redirect the guard surfaced. A Location pointing
// at a Cloudflare Access login means the service token is missing or not
// allowed on this hostname; any other redirect off the configured host is
// refused so request headers cannot leak, as is a scheme change on the same
// host — an https -> plaintext downgrade or an http -> https upgrade alike.
// The Location value is never printed — it can carry a signed login query.
func (c Client) redirectError(resp *http.Response) error {
	loc, e := url.Parse(resp.Header.Get("Location"))
	if e == nil {
		if cfAccessLoginHost(loc.Hostname()) {
			return &HTTPError{Status: resp.StatusCode, Code: "cf_access_login_redirect", Reason: "redirected to the Cloudflare Access login — the CF service token is missing or not allowed for this hostname"}
		}
		if loc.Host != "" {
			// loc.Host is set for absolute and scheme-relative Locations alike.
			base, be := url.Parse(strings.TrimRight(c.URL, "/"))
			if be == nil {
				switch {
				case normalizeHost(loc.Host) != normalizeHost(base.Host):
					return &HTTPError{Status: resp.StatusCode, Code: "redirect_foreign_host", Reason: "refused to follow a redirect away from the configured handoffkeep host"}
				case strings.EqualFold(base.Scheme, "https") && loc.IsAbs() && !strings.EqualFold(loc.Scheme, "https"):
					return &HTTPError{Status: resp.StatusCode, Code: "redirect_scheme_downgrade", Reason: "refused to follow a redirect that drops https for a plaintext scheme on the same host"}
				case loc.IsAbs() && !strings.EqualFold(loc.Scheme, base.Scheme):
					return &HTTPError{Status: resp.StatusCode, Code: "redirect_scheme_change", Reason: "refused to follow a redirect that leaves the configured scheme on the same host"}
				}
			}
		}
	}
	return &HTTPError{Status: resp.StatusCode, Code: "unexpected_redirect", Reason: "a JSON API call answered a redirect"}
}

// do issues r through cl and keeps transport errors leak-free: Go packs the
// raw Location header into the url.Error URL when CheckRedirect fails, and
// interpolates it verbatim when a Location fails to parse — both can carry a
// signed login query or other response-controlled data, so they collapse to
// named errors. What happened is recorded for this one request — the policy
// refusal or followed hop by wrapping cl's policy, the redirect response by
// wrapping its transport — never inferred from error text or the error URL,
// which Go rewrites (Location, redacted password). Every other error,
// including a failure after a hop back to the exact request URL, surfaces
// unchanged.
func (c Client) do(cl *http.Client, r *http.Request) (*http.Response, error) {
	hc := *cl
	policy := hc.CheckRedirect
	var refused error
	var hop *url.URL
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		var e error
		switch {
		case policy != nil:
			e = policy(req, via)
		case len(via) >= 10:
			e = ErrTooManyRedirects
		}
		switch {
		case e == nil:
			hop = req.URL
		case e != http.ErrUseLastResponse:
			refused = e
		}
		return e
	}
	// Client.Timeout treats a transport it does not know differently (legacy
	// cancel channel, racy timeout detection), so the recording transport is
	// only installed when the client has no Timeout; otherwise a Location
	// parse error must be one Go itself would produce for this request.
	var trace *redirectTrace
	if hc.Timeout == 0 {
		trace = &redirectTrace{next: hc.Transport}
		if trace.next == nil {
			trace.next = http.DefaultTransport
		}
		hc.Transport = trace
	}
	resp, e := hc.Do(r)
	if e == nil {
		return resp, nil
	}
	switch {
	case refused != nil:
		// The refusal comes back with the last response; Go sets the url.Error
		// URL to the raw Location, and a caller-supplied policy may wrap or
		// quote it, so no part of that error surfaces — only the wrapper's own
		// cap, recognized with errors.Is and returned as the bare sentinel.
		if errors.Is(refused, ErrTooManyRedirects) {
			return resp, ErrTooManyRedirects
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return resp, &HTTPError{Status: status, Code: "redirect_refused", Reason: "the redirect policy refused to follow a redirect from the configured handoffkeep host"}
	case hop != nil && hop.String() != r.URL.String():
		// A followed redirect hop to another URL failed before a response; the
		// error URL is derived from a response-controlled Location and must not
		// surface. A hop back to the exact request URL names only the
		// configured URL, so its error keeps Go's chain like any other.
		if !c.sameOriginHost(hop.Host) {
			return resp, &HTTPError{Code: "redirect_foreign_host", Reason: "a redirect hop failed away from the configured handoffkeep host"}
		}
		return resp, &HTTPError{Code: "redirect_location_invalid", Reason: "a redirect hop failed before a response was received"}
	}
	base := r.URL
	if hop != nil {
		base = hop
	}
	var ue *url.Error
	if (trace == nil || trace.located) && errors.As(e, &ue) && isLocationParseError(ue.Err, base) {
		return resp, &HTTPError{Code: "redirect_location_invalid", Reason: "the hk URL answered a redirect with an invalid Location header"}
	}
	return resp, e
}

// redirectTrace records whether the last round trip of one request answered
// a followable redirect status with a Location — the only state after which
// Go reports a Location parse error.
type redirectTrace struct {
	next    http.RoundTripper
	located bool
}

func (t *redirectTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, e := t.next.RoundTrip(r)
	t.located = e == nil && resp != nil && isRedirectStatus(resp.StatusCode) && resp.Header.Get("Location") != ""
	return resp, e
}

// isLocationParseError reports whether e is exactly the error Go's client
// returns when a Location answering a request for base does not parse: the
// quoted Location must itself fail to resolve against base with the same
// message, so an ordinary error that merely shares the prefix never matches.
func isLocationParseError(e error, base *url.URL) bool {
	rest, ok := strings.CutPrefix(e.Error(), "failed to parse Location header ")
	if !ok {
		return false
	}
	q, qe := strconv.QuotedPrefix(rest)
	if qe != nil {
		return false
	}
	loc, _ := strconv.Unquote(q)
	_, pe := base.Parse(loc)
	return pe != nil && rest == q+": "+pe.Error()
}

// htmlBodyError rejects a 2xx text/html answer on a JSON API call — the
// signature of a Cloudflare Access login page served instead of the API.
func htmlBodyError(resp *http.Response) error {
	mt, _, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e == nil && mt == "text/html" {
		return &HTTPError{Status: resp.StatusCode, Code: "unexpected_html_response", Reason: "the hk URL answered a text/html page instead of JSON — the request likely reached a Cloudflare Access login"}
	}
	return nil
}

func (c Client) call(ctx context.Context, method, path string, input, output any) error {
	var body *bytes.Reader
	if input != nil {
		b, e := json.Marshal(input)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	r, e := c.newRequest(ctx, method, path, body)
	if e != nil {
		return e
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.do(c.httpClient(), r)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if isRedirectStatus(resp.StatusCode) {
		return c.redirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct{ Error, Pattern, Reason string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return &HTTPError{Status: resp.StatusCode, Code: x.Error, Pattern: x.Pattern, Reason: x.Reason}
	}
	if e := htmlBodyError(resp); e != nil {
		return e
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	return nil
}

// HTTPError is a response the server actually sent with a non-2xx status.
// Its text is unchanged from the untyped errors it replaces, so callers that
// compare err.Error() keep working; callers that must know whether a write
// may have happened check the status instead. Any other error from call —
// transport failure, timeout, an undecodable 2xx body — means the outcome of
// a write is unknown: the server may have committed it.
type HTTPError struct {
	Status  int
	Code    string
	Pattern string
	Reason  string
}

func (e *HTTPError) Error() string {
	if e.Pattern != "" {
		return fmt.Sprintf("%s:%s", e.Code, e.Pattern)
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Reason)
	}
	if e.Code == "" {
		return fmt.Sprintf("http_%d", e.Status)
	}
	return e.Code
}

func esc(x string) string { return url.PathEscape(x) }
func (c Client) Checkpoint(ctx context.Context, _ string, x store.Checkpoint) (store.Checkpoint, error) {
	var out store.Checkpoint
	e := c.call(ctx, "POST", "/v1/checkpoints", x, &out)
	return out, e
}
func (c Client) Recent(ctx context.Context, session, kind string, limit int) ([]store.Checkpoint, error) {
	var out struct {
		Checkpoints []store.Checkpoint `json:"checkpoints"`
	}
	e := c.call(ctx, "GET", "/v1/checkpoints?session="+url.QueryEscape(session)+"&kind="+url.QueryEscape(kind)+fmt.Sprintf("&limit=%d", limit), nil, &out)
	return out.Checkpoints, e
}
func (c Client) PutMemory(ctx context.Context, _ string, x store.Memory) (store.Memory, error) {
	var out store.Memory
	e := c.call(ctx, "PUT", "/v1/memory/"+esc(x.Agent)+"/"+esc(x.Name), x, &out)
	return out, e
}
func (c Client) GetMemory(ctx context.Context, agent, name string) (store.Memory, bool, error) {
	var out store.Memory
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent)+"/"+esc(name), nil, &out)
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) ListMemory(ctx context.Context, agent string, content bool) ([]store.Memory, error) {
	var out struct {
		Memory []store.Memory `json:"memory"`
	}
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent), nil, &out)
	if e != nil || !content {
		return out.Memory, e
	}
	for i := range out.Memory {
		item, found, err := c.GetMemory(ctx, agent, out.Memory[i].Name)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("memory disappeared during pull")
		}
		out.Memory[i] = item
	}
	return out.Memory, nil
}
func (c Client) PutDocument(ctx context.Context, _ string, x store.Document) (store.Document, bool, error) {
	var out struct {
		Document store.Document `json:"document"`
		Changed  bool           `json:"changed"`
	}
	e := c.call(ctx, "PUT", "/v1/documents/"+esc(x.Key), x, &out)
	return out.Document, out.Changed, e
}
func (c Client) GetDocument(ctx context.Context, key string) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents/"+esc(key), nil, &out)
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) GetDocumentByID(ctx context.Context, id int64) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents?id="+fmt.Sprint(id), nil, &out)
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	if e != nil {
		return out, false, e
	}
	// A server older than this CLI ignores ?id= and returns a documents list,
	// which decodes to a zero Document — never report that as found. Any other
	// id mismatch means the server answered a different document.
	if out.ID != id {
		return out, false, fmt.Errorf("server returned document id %d for requested id %d: server may not support ?id= lookup (older than this CLI)", out.ID, id)
	}
	return out, true, nil
}
func (c Client) ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]store.Document, error) {
	var out struct {
		Documents []store.Document `json:"documents"`
	}
	p := url.Values{"prefix": {prefix}, "kind": {kind}, "session": {session}, "limit": {fmt.Sprint(limit)}}
	e := c.call(ctx, "GET", "/v1/documents?"+p.Encode(), nil, &out)
	return out.Documents, e
}

// searchWireCap is the largest limit the /v1/search API accepts
// (queryLimit def=20 max=100); requesting beyond it is a 400.
const searchWireCap = 100

func (c Client) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	var out struct {
		Results []store.SearchResult `json:"results"`
	}
	// Request one extra row so a cut page stays detectable against a server
	// that predates per-row truncated markers: len(results) > limit means more
	// rows exist. The API rejects limits above its 100 cap, so the probe only
	// applies strictly below it — at the cap the page cannot be probed.
	// limit < 1 is passed through unchanged.
	req := limit
	if req >= 1 && req < searchWireCap {
		req++
	}
	p := url.Values{"q": {q}, "scope": {scope}, "session": {session}, "limit": {fmt.Sprint(req)}}
	e := c.call(ctx, "GET", "/v1/search?"+p.Encode(), nil, &out)
	if e != nil {
		return nil, e
	}
	xs := out.Results
	if limit >= 1 && len(xs) > limit {
		xs = xs[:limit]
		for i := range xs {
			xs[i].Truncated = true
		}
	}
	return xs, nil
}
func (c Client) PutAttachment(ctx context.Context, _ string, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	r, e := c.newRequest(ctx, "PUT", "/v1/attachments", bytes.NewReader(body))
	if e != nil {
		return store.Attachment{}, false, e
	}
	r.Header.Set("X-HK-Name", name)
	r.Header.Set("Content-Type", mime)
	r.Header.Set("X-HK-Ref", ref)
	resp, e := c.do(c.httpClient(), r)
	if e != nil {
		return store.Attachment{}, false, e
	}
	defer resp.Body.Close()
	if isRedirectStatus(resp.StatusCode) {
		return store.Attachment{}, false, c.redirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, false, errors.New(x.Error)
	}
	if e := htmlBodyError(resp); e != nil {
		return store.Attachment{}, false, e
	}
	var x struct {
		Attachment store.Attachment `json:"attachment"`
		Created    bool             `json:"created"`
	}
	e = json.NewDecoder(resp.Body).Decode(&x)
	return x.Attachment, x.Created, e
}
func (c Client) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	var x struct {
		Attachments []store.Attachment `json:"attachments"`
	}
	e := c.call(ctx, "GET", "/v1/attachments?"+url.Values{"ref": {ref}, "limit": {fmt.Sprint(limit)}}.Encode(), nil, &x)
	return x.Attachments, e
}
func (c Client) AttachmentURL(ctx context.Context, sha string) (string, error) {
	r, e := c.newRequest(ctx, "GET", "/v1/attachments/"+esc(sha)+"?presign=1", nil)
	if e != nil {
		return "", e
	}
	resp, e := c.do(c.httpClientNoFollow(), r)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		loc := resp.Header.Get("Location")
		// A presign answer pointing at the Access login is the same
		// missing-token failure as on the JSON routes; a same-host scheme
		// change is a downgrade or upgrade outside the configured scheme,
		// not a usable storage URL. The Location value is never printed.
		if u, pe := url.Parse(loc); pe == nil && (cfAccessLoginHost(u.Hostname()) || c.sameHostSchemeChange(u)) {
			return "", c.redirectError(resp)
		}
		return loc, nil
	}
	if isRedirectStatus(resp.StatusCode) {
		return "", c.redirectError(resp)
	}
	return "", errors.New("attachment_url_failed")
}
func (c Client) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	r, e := c.newRequest(ctx, "GET", "/v1/attachments/"+esc(sha), nil)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	resp, e := c.do(c.httpClient(), r)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	if isRedirectStatus(resp.StatusCode) {
		defer resp.Body.Close()
		return store.Attachment{}, nil, c.redirectError(resp)
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		var x struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, nil, errors.New(x.Error)
	}
	// With the pair configured a 200 text/html that lacks an attachment
	// disposition is an Access login page, not file bytes — the real server
	// always sets Content-Disposition: attachment on this route, so a real
	// .html attachment still downloads. Without keys the historical
	// passthrough is unchanged.
	if c.cfAccessConfigured() {
		cd, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if !strings.EqualFold(cd, "attachment") {
			if e := htmlBodyError(resp); e != nil {
				defer resp.Body.Close()
				return store.Attachment{}, nil, e
			}
		}
	}
	return store.Attachment{SHA256: sha, MIME: resp.Header.Get("Content-Type")}, resp.Body, nil
}
func (c Client) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	var x struct {
		Usage store.AttachmentUsage `json:"usage"`
	}
	e := c.call(ctx, "GET", "/v1/usage", nil, &x)
	return x.Usage, e
}

func (c Client) CreateTask(ctx context.Context, x store.Task) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", "/v1/tasks", x, &out)
	if err != nil {
		// The server's code and reason surface unchanged — invalid_context on
		// create is a validation failure (missing lane, unknown kind), not a
		// version-skew verdict.
		return out, err
	}
	// A server that predates the project column could answer 200 while
	// silently dropping the field — same silent-drop defense as job_id.
	if x.Project != nil && (out.Project == nil || *out.Project != *x.Project) {
		return out, fmt.Errorf("create_project_not_recorded: server response has project unset — it likely predates the task project field")
	}
	return out, nil
}
func (c Client) ClaimTask(ctx context.Context, id int64, claimedBy, jobID, noJob string) (store.Task, error) {
	var out store.Task
	body := map[string]string{"claimed_by": claimedBy}
	if jobID != "" {
		body["job_id"] = jobID
	}
	if noJob != "" {
		body["no_job"] = noJob
	}
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/claim", id), body, &out)
	if err != nil {
		// Servers whose decoder rejects unknown fields answer the new body
		// with the generic invalid_context rather than naming job_id/no_job.
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			if jobID != "" {
				return out, fmt.Errorf("claim_job_id_rejected: server refused the claim with invalid_context (job_id=%q) — it likely predates job_id claim support", jobID)
			}
			if noJob != "" {
				return out, fmt.Errorf("claim_no_job_rejected: server refused the claim with invalid_context — it likely predates no_job claim support")
			}
		}
		return out, err
	}
	// A server that predates job_id claims can also answer 200 while
	// silently dropping the field. Treat that as failure: the task is
	// claimed but unlinked, and reporting success here would falsify the
	// job link.
	// A same-claimant replay on an already-active row answers with that
	// row's state (claimed or in_progress), so both are honest responses.
	if out.State != "claimed" && out.State != "in_progress" {
		return out, fmt.Errorf("claim_not_applied: server response has state=%q, want claimed", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("claim_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("claim_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
	}
	return out, nil
}
func (c Client) NextTask(ctx context.Context, lane, claimedBy, jobID, noJob string) (store.Task, error) {
	var out store.Task
	body := map[string]string{"lane": lane, "claimed_by": claimedBy}
	if jobID != "" {
		body["job_id"] = jobID
	}
	if noJob != "" {
		body["no_job"] = noJob
	}
	err := c.call(ctx, "POST", "/v1/tasks/next", body, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" && noJob != "" {
			return out, fmt.Errorf("next_no_job_rejected: server refused the claim with invalid_context — it likely predates no_job support")
		}
		return out, err
	}
	if out.State != "claimed" {
		return out, fmt.Errorf("next_not_applied: server response has state=%q, want claimed", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("next_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("next_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
	}
	return out, nil
}
func (c Client) TransitionTask(ctx context.Context, id int64, to, note string, refs *store.TaskRefs, noJob string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/transition", id), struct {
		To    string          `json:"to"`
		Note  string          `json:"note"`
		Refs  *store.TaskRefs `json:"refs,omitempty"`
		NoJob string          `json:"no_job,omitempty"`
	}{to, note, refs, noJob}, &out)
	if err != nil && noJob != "" {
		// Same strict-decoder translation as claims: a server that predates
		// the no_job field answers invalid_context rather than naming it.
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out, fmt.Errorf("transition_no_job_rejected: server refused the transition with invalid_context — it likely predates no_job support")
		}
	}
	if err != nil {
		return out, err
	}
	// A 200 that does not reflect the request is a lie, not a success: the
	// state must be the one asked for, and a job_id sent for linkage must be
	// recorded (same silent-drop defense as claims).
	if out.State != to {
		return out, fmt.Errorf("transition_not_applied: server response has state=%q, want %q", out.State, to)
	}
	// Landing in an active state without the exception means the response
	// must show the accountable pair — a pre-guard or lying server can 200
	// while leaving the row unlinked.
	if noJob == "" && (to == "claimed" || to == "in_progress") && (out.ClaimedBy == "" || out.Refs.JobID == "") {
		return out, fmt.Errorf("transition_linkage_missing: server response has claimed_by=%q refs.job_id=%q for an active transition", out.ClaimedBy, out.Refs.JobID)
	}
	if refs != nil && refs.JobID != "" && out.Refs.JobID != refs.JobID {
		return out, fmt.Errorf("transition_job_id_not_recorded: server response has refs.job_id=%q, want %q", out.Refs.JobID, refs.JobID)
	}
	return out, nil
}

// RecordDecisionRequest records a structured decision request on a task.
func (c Client) RecordDecisionRequest(ctx context.Context, id int64, in store.DecisionRequestInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request", id), in, &out)
	return out, err
}

// ResolveDecisionRequest closes a task's current decision request.
func (c Client) ResolveDecisionRequest(ctx context.Context, id int64, in store.DecisionResolveInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request/resolve", id), in, &out)
	return out, err
}

// RelaneResult is one item's outcome in a RelaneTasks response. The batch
// continues past item failures, so callers must inspect every entry.
type RelaneResult struct {
	ID      int64       `json:"id"`
	OK      bool        `json:"ok"`
	Changed bool        `json:"changed"`
	Task    *store.Task `json:"task,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// RelaneBatch is the full relane response: per-item results plus the moved /
// unchanged / failed tallies the server counted.
type RelaneBatch struct {
	Results   []RelaneResult `json:"results"`
	Moved     int            `json:"moved"`
	Unchanged int            `json:"unchanged"`
	Failed    int            `json:"failed"`
}

// RelaneTasks moves tasks between lanes. A pre-relane server has no route:
// its mux matches GET /v1/tasks/{id} on the path and answers 405, which
// call surfaces as http_405 via its status fallback.
func (c Client) RelaneTasks(ctx context.Context, ids []int64, to, note string, allowNewLane bool) (RelaneBatch, error) {
	var out RelaneBatch
	err := c.call(ctx, "POST", "/v1/tasks/relane", struct {
		IDs          []int64 `json:"ids"`
		To           string  `json:"to"`
		Note         string  `json:"note"`
		AllowNewLane bool    `json:"allow_new_lane,omitempty"`
	}{ids, to, note, allowNewLane}, &out)
	return out, err
}
func (c Client) CreateDisposition(ctx context.Context, x store.DispositionInput) (store.Task, bool, error) {
	var out struct {
		Task    store.Task `json:"task"`
		Created bool       `json:"created"`
	}
	err := c.call(ctx, "POST", "/v1/tasks/dispositions", x, &out)
	if err != nil && x.Project != "" {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out.Task, false, fmt.Errorf("disposition_project_rejected: server refused the disposition with invalid_context — it likely predates the task project field")
		}
	}
	return out.Task, out.Created, err
}
func (c Client) DispositionSummary(ctx context.Context, asOf string) (store.DispositionSummary, error) {
	var out store.DispositionSummary
	path := "/v1/tasks/dispositions/summary"
	if asOf != "" {
		path += "?" + url.Values{"as_of": {asOf}}.Encode()
	}
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}
func (c Client) ApplyDisposition(ctx context.Context, id int64, note string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/dispositions/%d/apply", id), map[string]string{"note": note}, &out)
	return out, err
}

// taskProjectFilterOK reports the rows a project-filtered list may contain.
// A server that predates the project column silently ignores the parameter,
// so every returned row must match the requested filter — nil project never
// equals a named filter.
func taskProjectFilterOK(xs []store.Task, project *string) error {
	if project == nil {
		return nil
	}
	for _, t := range xs {
		if *project == "" {
			if t.Project != nil {
				return fmt.Errorf("list_project_filter_ignored: task %d has project %q — the server predates --project filtering", t.ID, *t.Project)
			}
			continue
		}
		if t.Project == nil || *t.Project != *project {
			return fmt.Errorf("list_project_filter_ignored: task %d does not match project %q — the server predates --project filtering", t.ID, *project)
		}
	}
	return nil
}

// taskProjectEchoOK requires the list response to echo the requested filter.
// A server that predates the project column silently drops the parameter and
// cannot echo it — and an empty page has no rows for taskProjectFilterOK to
// inspect, so the echo is the only reliable old-server signal there.
func taskProjectEchoOK(echo *string, project *string) error {
	if project == nil {
		return nil
	}
	if echo == nil || *echo != *project {
		return fmt.Errorf("list_project_filter_ignored: server response lacks the project echo — it predates --project filtering")
	}
	return nil
}

func (c Client) ListTasks(ctx context.Context, lane, state, parentLane string, project *string, limit int) ([]store.Task, error) {
	var out struct {
		Tasks   []store.Task `json:"tasks"`
		Project *string      `json:"project"`
	}
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}, "limit": {fmt.Sprint(limit)}}
	if project != nil {
		q.Set("project", *project)
	}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectEchoOK(out.Project, project); err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}
func (c Client) ListTasksPage(ctx context.Context, lane, state, parentLane string, project *string, afterID int64, limit int) ([]store.Task, error) {
	var out struct {
		Tasks   []store.Task `json:"tasks"`
		Project *string      `json:"project"`
	}
	q := url.Values{
		"lane":        {lane},
		"state":       {state},
		"parent_lane": {parentLane},
		"after_id":    {fmt.Sprint(afterID)},
		"limit":       {fmt.Sprint(limit)},
	}
	if project != nil {
		q.Set("project", *project)
	}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectEchoOK(out.Project, project); err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// ListTaskProjects answers the server-configured project vocabulary. A
// server that predates the endpoint either 404/405s the route or — for GET —
// lands on /v1/tasks/{id} with id="projects" and answers invalid_context.
func (c Client) ListTaskProjects(ctx context.Context) ([]string, error) {
	var out struct {
		Projects []string `json:"projects"`
	}
	err := c.call(ctx, "GET", "/v1/tasks/projects", nil, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405 || he.Code == "invalid_context") {
			return nil, fmt.Errorf("task_projects_unsupported: server predates the task project vocabulary endpoint")
		}
		return nil, err
	}
	return out.Projects, nil
}

// AddTaskProject extends the server-configured project vocabulary. created
// is false when the name already exists. A server that predates the route
// answers 404/405 — an invalid name is a real invalid_context rejection and
// is not translated.
func (c Client) AddTaskProject(ctx context.Context, name string) (bool, error) {
	var out struct {
		Created bool `json:"created"`
	}
	err := c.call(ctx, "POST", "/v1/tasks/projects", map[string]string{"name": name}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return false, fmt.Errorf("task_projects_unsupported: server predates the task project vocabulary endpoint")
		}
		return false, err
	}
	return out.Created, nil
}

// SetTaskProject reclassifies one task inside the server's configured
// project vocabulary and returns the updated row. A server that predates the
// route answers 404/405, surfaced as task_project_unsupported.
func (c Client) SetTaskProject(ctx context.Context, id int64, project, note string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/project", id), map[string]string{"project": project, "note": note}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return out, fmt.Errorf("task_project_unsupported: server predates the task project route")
		}
		return out, err
	}
	if out.Project == nil || *out.Project != project {
		return out, fmt.Errorf("project_not_applied: server response has project unset — it likely predates the task project field")
	}
	return out, nil
}

// ExportTasks fetches one consistent task snapshot and returns the exact
// response body so callers print the evidence document byte-for-byte. A
// limit below 1 omits the parameter so the server default applies.
func (c Client) ExportTasks(ctx context.Context, lane, state, parentLane string, project *string, limit int) ([]byte, error) {
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}}
	if project != nil {
		q.Set("project", *project)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	r, e := c.newRequest(ctx, "GET", "/v1/tasks/export?"+q.Encode(), nil)
	if e != nil {
		return nil, e
	}
	resp, e := c.do(c.httpClient(), r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if isRedirectStatus(resp.StatusCode) {
		return nil, c.redirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return nil, errors.New(x.Error)
	}
	if e := htmlBodyError(resp); e != nil {
		return nil, e
	}
	body, e := io.ReadAll(resp.Body)
	if e != nil {
		return nil, e
	}
	// A requested project filter must echo back inside scope. A server that
	// predates the field silently ignores the parameter and returns an
	// unfiltered export — the byte-for-byte document would carry no signal.
	if project != nil {
		var probe struct {
			Scope struct {
				Project *string `json:"project"`
			} `json:"scope"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			return nil, fmt.Errorf("export_project_filter_unchecked: server export body did not parse — cannot confirm the project filter was applied: %w", err)
		}
		if probe.Scope.Project == nil || *probe.Scope.Project != *project {
			return nil, fmt.Errorf("export_project_filter_ignored: server export lacks scope.project=%q — it predates --project filtering", *project)
		}
	}
	return body, nil
}
func (c Client) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	var out store.Task
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d", id), nil, &out)
	if err != nil && err.Error() == "not_found" {
		return out, false, nil
	}
	return out, err == nil, err
}

func (c Client) LinearOutboxStatus(ctx context.Context) (store.LinearOutboxStatus, error) {
	var out store.LinearOutboxStatus
	err := c.call(ctx, "GET", "/v1/linear/status", nil, &out)
	return out, err
}

// ResolveDecision closes an already-handled decision through the bearer API.
func (c Client) ResolveDecision(ctx context.Context, kind string, id int64, by, answer, note string, noInject bool, noJob string) (store.RelayEvent, error) {
	var out struct {
		Event store.RelayEvent `json:"event"`
	}
	err := c.call(ctx, "POST", "/v1/decisions/resolve", struct {
		Type     string `json:"type"`
		ID       int64  `json:"id"`
		By       string `json:"by"`
		Answer   string `json:"answer"`
		Note     string `json:"note,omitempty"`
		NoInject bool   `json:"no_inject,omitempty"`
		NoJob    string `json:"no_job,omitempty"`
	}{kind, id, by, answer, note, noInject, noJob}, &out)
	if err != nil && noJob != "" {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out.Event, fmt.Errorf("resolve_no_job_rejected: server refused the resolve with invalid_context — it likely predates no_job support")
		}
	}
	return out.Event, err
}

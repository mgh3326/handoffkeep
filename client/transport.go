package client

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
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/buildinfo"
)

// UserAgent is stamped on every hk request: Cloudflare Access can be
// configured to block the default Go user agent before a request reaches the
// service, and a distinct UA keeps hk calls identifiable in Access logs.
var UserAgent = "handoffkeep/" + buildinfo.Revision()

// Client is one hk API caller. The caller sets URL and Token, and the optional
// Cloudflare Access pair; the package never loads them (see the package doc).
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

// cfAccessConfigured reports whether the service-token pair is present;
// whitespace-only values count as unset.
func (c Client) cfAccessConfigured() bool {
	return strings.TrimSpace(c.CFAccessClientID) != "" && strings.TrimSpace(c.CFAccessClientSecret) != ""
}

// NewRequest builds one request against the configured hk URL carrying every
// header an hk call sends: bearer auth, the explicit User-Agent, and — only
// when both are configured — the Cloudflare Access service-token pair.
func (c Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
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

// HTTPClientNoFollow never follows a redirect: the presign answer must surface
// its Location to the caller rather than forward the CF Access headers — or
// any headers — to the storage host.
func (c Client) HTTPClientNoFollow() *http.Client {
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	hc := *h
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// IsRedirectStatus reports the 3xx statuses whose Location a client may follow.
func IsRedirectStatus(code int) bool {
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

// RedirectError classifies a redirect the guard surfaced. A Location pointing
// at a Cloudflare Access login means the service token is missing or not
// allowed on this hostname; any other redirect off the configured host is
// refused so request headers cannot leak, as is a scheme change on the same
// host — an https -> plaintext downgrade or an http -> https upgrade alike.
// The Location value is never printed — it can carry a signed login query.
func (c Client) RedirectError(resp *http.Response) error {
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

// Do issues r, built by NewRequest, through the redirect-guarded client: a
// redirect is followed only on the configured host and scheme, at most ten
// hops, and transport errors are kept leak-free as do describes.
func (c Client) Do(r *http.Request) (*http.Response, error) {
	return c.do(c.httpClient(), r)
}

// do issues r through cl and keeps transport errors leak-free: Go packs the
// raw Location header into the url.Error URL when CheckRedirect fails, and
// interpolates it verbatim when a Location fails to parse — both can carry a
// signed login query or other response-controlled data, so they collapse to
// named errors. What happened is recorded for this one request — the policy
// refusal or followed hop by wrapping cl's policy, the redirect response by
// wrapping its transport — never inferred from the error URL, which Go
// rewrites (Location, redacted password). Only a caller client with a
// positive Client.Timeout falls back to matching the parse-error text (see
// isLocationParseError). Every other error, including a failure after a hop
// back to the exact request URL, surfaces unchanged.
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
	// A positive Client.Timeout treats a transport it does not know
	// differently (legacy cancel channel, racy timeout detection), so the
	// recording transport is installed only when the client has no deadline —
	// Go ignores a nonpositive Timeout. Under a positive Timeout the Location
	// parse error is matched by its canonical text instead: noncanonical
	// alternate escaping is not matched, though Go's own percent-q rendering
	// can itself contain hex or unicode escapes for control characters, and
	// those are accepted.
	var trace *redirectTrace
	if hc.Timeout <= 0 {
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
	t.located = e == nil && resp != nil && IsRedirectStatus(resp.StatusCode) && resp.Header.Get("Location") != ""
	return resp, e
}

// isLocationParseError reports whether e has the canonical rendering of
// Go's Location parse error for a request to base — "failed to parse Location
// header " then the Location as fmt %q renders it (strconv.Quote; noncanonical
// alternate escaping — raw-string or rune quoting, or a hand-written hex or
// unicode escape strconv.Quote would not emit — is rejected, though Go's own
// percent-q rendering can itself contain hex or unicode escapes for control
// characters, and those are accepted), then ": " and the error base.Parse
// itself returns for that Location. Matching the text does not prove Go wrote
// it: under a positive Client.Timeout, where no response is recorded, a
// caller transport that forges this exact text for an unparseable Location is
// classified redirect_location_invalid — a fixed, leak-free error. That
// excluded case is unreachable from the default or shipped CLI and stdio,
// which never set a Timeout and always record the redirect response; a library
// caller can pass its own positive-timeout client to NewStdio.
func isLocationParseError(e error, base *url.URL) bool {
	rest, ok := strings.CutPrefix(e.Error(), "failed to parse Location header ")
	if !ok {
		return false
	}
	q, qe := strconv.QuotedPrefix(rest)
	if qe != nil {
		return false
	}
	loc, ue := strconv.Unquote(q)
	if ue != nil || q != strconv.Quote(loc) {
		return false
	}
	_, pe := base.Parse(loc)
	return pe != nil && rest == q+": "+pe.Error()
}

// HTMLBodyError rejects a 2xx text/html answer on a JSON API call — the
// signature of a Cloudflare Access login page served instead of the API.
func HTMLBodyError(resp *http.Response) error {
	mt, _, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e == nil && mt == "text/html" {
		return &HTTPError{Status: resp.StatusCode, Code: "unexpected_html_response", Reason: "the hk URL answered a text/html page instead of JSON — the request likely reached a Cloudflare Access login"}
	}
	return nil
}

// Call sends input (nil for no body) as JSON to method path and decodes a 2xx
// answer into output (nil to discard it). A non-2xx answer is an *HTTPError
// carrying the server's error code.
func (c Client) Call(ctx context.Context, method, path string, input, output any) error {
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
	r, e := c.NewRequest(ctx, method, path, body)
	if e != nil {
		return e
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.Do(r)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if IsRedirectStatus(resp.StatusCode) {
		return c.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error, Pattern, Reason string
			SimilarTasks           []SimilarTask `json:"similar_tasks"`
			Reset                  bool
			MinSyncID              int64 `json:"min_sync_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return &HTTPError{Status: resp.StatusCode, Code: x.Error, Pattern: x.Pattern, Reason: x.Reason, SimilarTasks: x.SimilarTasks, Reset: x.Reset, MinSyncID: x.MinSyncID}
	}
	if e := HTMLBodyError(resp); e != nil {
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
	// SimilarTasks lists the open tasks a 409 similar_tasks found.
	SimilarTasks []SimilarTask
	// Reset and MinSyncID carry a change-feed reader's expired-cursor answer
	// (the ledger): 410 {"reset":true,"min_sync_id":N}.
	Reset     bool
	MinSyncID int64
}

// SimilarTask is an open task whose title is similar to a new one, as a 409
// similar_tasks refusal lists it.
type SimilarTask struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	State      string  `json:"state"`
	Lane       string  `json:"lane"`
	Similarity float64 `json:"similarity"`
}

func (e *HTTPError) Error() string {
	if len(e.SimilarTasks) > 0 {
		parts := make([]string, 0, len(e.SimilarTasks))
		for _, x := range e.SimilarTasks {
			parts = append(parts, fmt.Sprintf("#%d %q (%s)", x.ID, x.Title, x.State))
		}
		return "similar_tasks: " + strings.Join(parts, "; ")
	}
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

// ErrorCode returns the server's error code ("not_found", "queue_empty", ...)
// when err is an *HTTPError, and "" otherwise. Match codes with it, never by
// comparing err.Error() text.
func ErrorCode(err error) string {
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Code
	}
	return ""
}

// SecretPattern reports the pattern the server named when it refused a write
// as secret_like_content.
func SecretPattern(err error) (string, bool) {
	var h *HTTPError
	if errors.As(err, &h) && h.Code == "secret_like_content" {
		return h.Pattern, true
	}
	return "", false
}

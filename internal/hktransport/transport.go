// Package hktransport is hk's one /v1 transport: the request builder, the
// redirect-guarded and no-follow senders, the JSON call and the error
// decoding. Package client (the fleet calls) and internal/remote (the CLI's
// calls) both send through it.
//
// It lives under internal/ so that no other module can build or send a raw
// hk request: a request carries the bearer token and the Cloudflare Access
// pair, so every one is held to the configured origin — the builder accepts
// only a single-slash absolute path and checks the joined URL, both senders
// refuse a request that could leave the configured origin before any network
// I/O, and the redirect guard judges every hop the same way — all through one
// origin-equality check, sameOrigin (hk 3544).
//
// It imports only the standard library and internal/buildinfo, because
// package client, which the fleet services import, imports it.
package hktransport

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
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/buildinfo"
)

// UserAgent is stamped on every hk request: Cloudflare Access can be
// configured to block the default Go user agent before a request reaches the
// service, and a distinct UA keeps hk calls identifiable in Access logs.
var UserAgent = "handoffkeep/" + buildinfo.Revision()

// Client is one hk API caller. The caller sets URL and Token, and the optional
// Cloudflare Access pair; the package never loads them. URL is an absolute
// http or https URL with an ASCII host (punycode for an internationalized
// name) and an optional path prefix. Its fields match
// client.Client's field for field, so each converts to the other.
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

// CFAccessConfigured reports whether the service-token pair is present;
// whitespace-only values count as unset.
func (c Client) CFAccessConfigured() bool {
	return strings.TrimSpace(c.CFAccessClientID) != "" && strings.TrimSpace(c.CFAccessClientSecret) != ""
}

// ErrInvalidURL refuses a configured URL that is not an absolute http or
// https URL with an ASCII host. The URL is not printed.
var ErrInvalidURL = errors.New("hk_url_invalid: the configured hk URL must be an absolute http or https URL with an ASCII host")

// ErrInvalidPath refuses a request path that is not a single-slash absolute
// path: joined onto the URL, "@host/", "//host/" or "scheme://host/" names
// another host. The path is not printed.
var ErrInvalidPath = errors.New("hk_request_path_invalid: an hk request path must start with a single /")

// ErrForeignOrigin refuses, before any network I/O, a request that could
// leave the configured origin (see sendable): it would carry the bearer token
// and the CF Access pair elsewhere.
var ErrForeignOrigin = errors.New("hk_request_foreign_origin: refused to send an hk request outside the configured scheme and host")

// base parses the configured URL with its trailing slashes removed — the
// form a request path is joined onto. It refuses an opaque URL and a host
// that normalizeHost cannot fold — empty or with a non-ASCII byte.
func (c Client) base() (*url.URL, error) {
	u, e := url.Parse(strings.TrimRight(c.URL, "/"))
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || normalizeHost(u.Host) == "" {
		return nil, ErrInvalidURL
	}
	return u, nil
}

// sameOrigin is the one origin-equality check: the builder (NewRequest), both
// senders (Do, DoNoFollow) and the redirect guard (httpClient) call it, so
// they cannot drift apart. It reports whether u stays on the configured
// origin:
//   - the configured URL is valid (base);
//   - u is not opaque — through a forward proxy an opaque value is the
//     request target, whatever host u names;
//   - u carries exactly the configured URL's userinfo: none when the
//     configured URL has none, so a request cannot gain credentials or a
//     confusable "user@" prefix the configuration did not hold;
//   - u's scheme equals the configured scheme, ASCII case folded;
//   - u's host equals the configured host as hostEqual compares them: byte
//     for byte after ASCII case folding and dropping a trailing dot, an IPv6
//     zone byte-exact, and the port as written — "hk.example:443" and
//     "hk.example" differ, as they did before. A host with any non-ASCII
//     byte equals nothing: Go's transport maps it through IDNA to a DNS name
//     Unicode case folding cannot predict (İ is not i), so the configured
//     host must be ASCII, punycode for an internationalized name.
func (c Client) sameOrigin(u *url.URL) bool {
	base, e := c.base()
	return e == nil && u != nil && u.Opaque == "" && sameUserinfo(u.User, base.User) && asciiEqualFold(u.Scheme, base.Scheme) && hostEqual(u.Host, base.Host)
}

// sameUserinfo reports whether a and b are both absent or carry the same
// username and password, byte for byte.
func sameUserinfo(a, b *url.Userinfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	ap, aset := a.Password()
	bp, bset := b.Password()
	return a.Username() == b.Username() && ap == bp && aset == bset
}

// sendable reports whether r may be sent: its URL is on the configured origin
// (sameOrigin) and its Host header field, when set, is exactly the URL's host —
// through a forward proxy the Host field becomes the request target.
func (c Client) sendable(r *http.Request) bool {
	return r != nil && c.sameOrigin(r.URL) && (r.Host == "" || r.Host == r.URL.Host)
}

// NewRequest builds one request against the configured hk URL carrying every
// header an hk call sends: bearer auth, the explicit User-Agent, and — only
// when both are configured — the Cloudflare Access service-token pair. path
// is joined onto the URL (a path prefix on the URL is kept), so it must start
// with a single "/"; the joined URL must keep the configured scheme and host.
// Otherwise NewRequest returns an error and no request.
func (c Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if _, e := c.base(); e != nil {
		return nil, e
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, ErrInvalidPath
	}
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if e != nil {
		return nil, e
	}
	if !c.sameOrigin(r.URL) {
		return nil, ErrForeignOrigin
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("User-Agent", UserAgent)
	if id, secret := strings.TrimSpace(c.CFAccessClientID), strings.TrimSpace(c.CFAccessClientSecret); id != "" && secret != "" {
		r.Header.Set("CF-Access-Client-Id", id)
		r.Header.Set("CF-Access-Client-Secret", secret)
	}
	return r, nil
}

// hostEqual reports whether two URL Hosts name the same origin host: both
// fold under normalizeHost to the same non-empty string. A host with a
// non-ASCII byte folds to "" and so equals nothing, not even itself.
func hostEqual(a, b string) bool {
	na := normalizeHost(a)
	return na != "" && na == normalizeHost(b)
}

// normalizeHost folds a URL Host (hostname[:port]) to the form DNS treats as
// equal: ASCII lowercase with a trailing FQDN dot removed, the port kept as
// written. It returns "" for an empty host or one with any non-ASCII byte.
func normalizeHost(h string) string {
	if host, port, e := net.SplitHostPort(h); e == nil {
		if f := foldHostname(host); f != "" && isASCII(port) {
			return f + ":" + port
		}
		return ""
	}
	return foldHostname(h)
}

// foldHostname folds one hostname, or returns "" when it is empty or holds a
// non-ASCII byte. The zone of an IPv6 literal (fe80::1%zone) names an
// interface, which Go resolves case-sensitively, so only the address part is
// folded and the zone is kept byte-exact — a zone that differs, even by case
// alone, is a different origin.
func foldHostname(h string) string {
	addr, zone := h, ""
	if strings.Contains(h, ":") {
		if i := strings.IndexByte(h, '%'); i >= 0 {
			addr, zone = h[:i], h[i:]
		}
	}
	folded, ok := asciiLower(strings.TrimSuffix(addr, "."))
	if !ok || folded == "" || !isASCII(zone) {
		return ""
	}
	return folded + zone
}

// asciiLower lowercases A-Z and reports false for any non-ASCII byte. Unicode
// lowercasing (strings.ToLower) is not used: it folds İ to i and the Kelvin
// sign to k, which name other hosts on the wire.
func asciiLower(s string) (string, bool) {
	b := []byte(s)
	for i, ch := range b {
		switch {
		case ch >= utf8.RuneSelf:
			return "", false
		case 'A' <= ch && ch <= 'Z':
			b[i] = ch + ('a' - 'A')
		}
	}
	return string(b), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// asciiEqualFold compares two schemes with ASCII case folding only.
func asciiEqualFold(a, b string) bool {
	la, oka := asciiLower(a)
	lb, okb := asciiLower(b)
	return oka && okb && la == lb
}

// httpClient returns the client for one hk request wrapped so a redirect that
// would move the Authorization or CF Access headers off the configured origin
// is never followed — Go re-sends custom headers on redirects. Every hop is
// judged by sameOrigin against the configured URL, not against the chain's
// first request. An https -> plaintext hop is refused for the same reason as
// a cross-host hop, as is an http -> https upgrade: credentials are scoped to
// the configured scheme, not just the host. When the wrapped client has no policy
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
		if !c.sameOrigin(req.URL) {
			return http.ErrUseLastResponse
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

// noFollowClient never follows a redirect: the presign answer must surface
// its Location to the caller rather than forward the CF Access headers — or
// any headers — to the storage host. It is used only through DoNoFollow, so
// its errors pass through do.
func (c Client) noFollowClient() *http.Client {
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

// CFAccessLoginHost reports whether host is a Cloudflare Access login host
// (<team>.cloudflareaccess.com or the apex). DNS names compare
// case-insensitively with an optional trailing dot.
func CFAccessLoginHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "cloudflareaccess.com" || strings.HasSuffix(host, ".cloudflareaccess.com")
}

// sameOriginHost reports whether h names the configured hk host, as
// sameOrigin compares hosts (hostEqual).
func (c Client) sameOriginHost(h string) bool {
	base, e := c.base()
	return e == nil && hostEqual(h, base.Host)
}

// SameHostSchemeChange reports whether u stays on the configured hk host but
// switches scheme — a redirect in either direction (upgrade or downgrade)
// moves the request outside the scheme the credentials were configured for.
func (c Client) SameHostSchemeChange(u *url.URL) bool {
	if !u.IsAbs() || !c.sameOriginHost(u.Host) {
		return false
	}
	base, e := c.base()
	return e == nil && !asciiEqualFold(u.Scheme, base.Scheme)
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
		if CFAccessLoginHost(loc.Hostname()) {
			return &HTTPError{Status: resp.StatusCode, Code: "cf_access_login_redirect", Reason: "redirected to the Cloudflare Access login — the CF service token is missing or not allowed for this hostname"}
		}
		if loc.Host != "" {
			// loc.Host is set for absolute and scheme-relative Locations alike.
			base, be := c.base()
			if be == nil {
				switch {
				case !hostEqual(loc.Host, base.Host):
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
// redirect is followed only on the configured origin, at most ten hops, and
// transport errors are kept leak-free as do describes. A request that could
// leave the configured origin — scheme or host not the configured one, an
// opaque URL, userinfo other than the configured URL's, or a Host field other
// than the URL's host — is
// refused with ErrForeignOrigin before any network I/O.
func (c Client) Do(r *http.Request) (*http.Response, error) {
	if !c.sendable(r) {
		return nil, ErrForeignOrigin
	}
	return c.do(c.httpClient(), r)
}

// DoNoFollow issues r, built by NewRequest, without following any redirect:
// a 3xx answer is returned with its Location for the caller to judge, and no
// header reaches the Location's host. The origin check and the leak-free
// errors are Do's.
func (c Client) DoNoFollow(r *http.Request) (*http.Response, error) {
	if !c.sendable(r) {
		return nil, ErrForeignOrigin
	}
	return c.do(c.noFollowClient(), r)
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

package client

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/hktransport"
)

// UserAgent is stamped on every hk request: Cloudflare Access can be
// configured to block the default Go user agent before a request reaches the
// service, and a distinct UA keeps hk calls identifiable in Access logs.
var UserAgent = hktransport.UserAgent

// Client is one hk API caller. The caller sets URL and Token, and the optional
// Cloudflare Access pair; the package never loads them (see the package doc).
// URL is an absolute http or https URL with a host; every call is sent only
// to its scheme and host.
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

// transport is c as hk's module-internal transport, which every call sends
// through. The two structs share their fields, so the conversion cannot drift
// without breaking the build.
func (c Client) transport() hktransport.Client { return hktransport.Client(c) }

// ErrTooManyRedirects is the transport's own redirect cap: a same-host loop
// stops after Go's default ten hops. It is the only redirect-policy error
// returned as is; callers recognize it with errors.Is.
var ErrTooManyRedirects = hktransport.ErrTooManyRedirects

// HTTPError is a response the server actually sent with a non-2xx status, or
// a redirect or response the client refused, with a named code. Any other
// error from a call — transport failure, timeout, an undecodable 2xx body —
// means the outcome of a write is unknown: the server may have committed it.
type HTTPError = hktransport.HTTPError

// SimilarTask is an open task whose title is similar to a new one, as a 409
// similar_tasks refusal lists it (HTTPError.SimilarTasks).
type SimilarTask = hktransport.SimilarTask

// ErrorCode returns the server's error code ("not_found", "queue_empty", ...)
// when err is an *HTTPError, and "" otherwise. Match codes with it, never by
// comparing err.Error() text.
func ErrorCode(err error) string { return hktransport.ErrorCode(err) }

// SecretPattern reports the pattern the server named when it refused a write
// as secret_like_content.
func SecretPattern(err error) (string, bool) { return hktransport.SecretPattern(err) }

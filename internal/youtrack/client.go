// Package youtrack is hk's small client for the operator's YouTrack screen and
// the two-way sync that keeps the mirror current. It never logs or returns the
// bearer token; every request carries it in a header only.
package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults are intentionally conservative: one YouTrack call is ~0.3s, and a
// 2-minute sync must finish well inside its window even when YouTrack is slow.
const (
	defaultTimeout    = 10 * time.Second
	defaultMaxRetries = 3
	defaultBackoff    = 200 * time.Millisecond
	maxRetryWait      = 5 * time.Second
	maxBodyBytes      = 4 << 20
)

var (
	ErrConfig         = errors.New("youtrack_config")
	ErrNotFound       = errors.New("youtrack_not_found")
	ErrUnavailable    = errors.New("youtrack_unavailable")
	ErrFieldNotFound  = errors.New("youtrack_field_not_found")
	ErrProjectMissing = errors.New("youtrack_project_missing")
)

// Config is the client's only input. Token is never rendered by String or by
// an error; the zero value is refused by New.
type Config struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Timeout    time.Duration
	MaxRetries int
	Backoff    time.Duration
	UserAgent  string
	// Sleep is the retry wait, injectable so tests do not spend real time.
	Sleep func(ctx context.Context, d time.Duration) error
}

type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	timeout   time.Duration
	retries   int
	backoff   time.Duration
	userAgent string
	sleep     func(context.Context, time.Duration) error

	meOnce sync.Once
	me     User
	meErr  error
}

func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" || strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("%w: base URL and token are required", ErrConfig)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base URL is not an absolute URL", ErrConfig)
	}
	if parsed.Scheme != "https" && !loopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("%w: base URL must be https (http is allowed only for loopback test servers)", ErrConfig)
	}
	c := &Client{
		baseURL:   base,
		token:     cfg.Token,
		http:      cfg.HTTPClient,
		timeout:   cfg.Timeout,
		retries:   cfg.MaxRetries,
		backoff:   cfg.Backoff,
		userAgent: cfg.UserAgent,
		sleep:     cfg.Sleep,
	}
	// The bearer token follows only redirects that stay on the configured
	// origin. The client is copied so the caller's transport is kept.
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	hc.CheckRedirect = sameOriginRedirects(originOf(parsed), hc.CheckRedirect)
	c.http = hc
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.retries < 0 {
		c.retries = 0
	} else if c.retries == 0 {
		c.retries = defaultMaxRetries
	}
	if c.backoff <= 0 {
		c.backoff = defaultBackoff
	}
	if c.userAgent == "" {
		c.userAgent = "handoffkeep-youtrack-sync"
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	return c, nil
}

// maxRedirects matches net/http's own limit, which applies when the caller's
// client has no CheckRedirect of its own.
const maxRedirects = 10

// sameOriginRedirects refuses a redirect to another scheme, host or port.
// Net/http strips Authorization on some host changes but still follows the
// redirect and reads its answer; the client never does that with the token.
func sameOriginRedirects(origin string, next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if originOf(req.URL) != origin {
			return fmt.Errorf("youtrack: refused redirect to other origin %s", originOf(req.URL))
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("youtrack: stopped after 10 redirects")
		}
		return nil
	}
}

// originOf is scheme://host:port with the default port filled in, so
// https://a and https://a:443 compare equal and https://a and http://a do not.
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// APIError is one non-2xx answer. It carries no request headers or body text
// that could contain the token.
type APIError struct {
	Status  int
	Code    string
	Message string
	Method  string
	Path    string
}

func (e *APIError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("youtrack %s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Code, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("youtrack %s %s: %d %s", e.Method, e.Path, e.Status, e.Code)
	}
	return fmt.Sprintf("youtrack %s %s: http %d", e.Method, e.Path, e.Status)
}

func (e *APIError) Temporary() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// ErrorCode returns the API error's stable code, or "".
func ErrorCode(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

// IsNotFound reports a 404 from YouTrack.
func IsNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// IsTemporary reports an answer worth retrying (429 or 5xx).
func IsTemporary(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Temporary()
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, retry5xx bool) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		data, retryAfter, err := c.once(ctx, method, endpoint, payload)
		if err != nil {
			retryable := false
			if apiStatus(err) == http.StatusTooManyRequests {
				// A rate-limited request was not processed.
				retryable = true
			} else if IsTemporary(err) {
				retryable = retry5xx
			} else if retryableTransport(err) {
				// A dropped connection after a create may already have
				// committed; only idempotent requests retry it.
				retryable = retry5xx
			}
			if attempt < c.retries && retryable {
				if werr := c.sleep(ctx, c.backoffFor(attempt, retryAfter)); werr != nil {
					return werr
				}
				continue
			}
			return err
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("youtrack %s %s: decode: %w", method, path, err)
			}
		}
		return nil
	}
}

func (c *Client) once(ctx context.Context, method, endpoint string, payload []byte) ([]byte, time.Duration, error) {
	callCtx := ctx
	if c.timeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(callCtx, method, endpoint, body)
	if err != nil {
		return nil, 0, fmt.Errorf("youtrack %s: %w", method, redact(err, c.token))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("youtrack %s %s: %w", method, pathOf(endpoint), redact(err, c.token))
	}
	defer resp.Body.Close()
	// One byte past the cap tells an over-size answer from one that fits. A
	// truncated body can still be valid JSON (a list followed by padding), so
	// it is refused, never decoded.
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if readErr != nil {
		return nil, 0, fmt.Errorf("youtrack %s %s: read: %w", method, pathOf(endpoint), readErr)
	}
	if len(data) > maxBodyBytes {
		return nil, 0, fmt.Errorf("youtrack %s %s: response larger than %d bytes", method, pathOf(endpoint), maxBodyBytes)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, 0, nil
	}
	apiErr := decodeAPIError(resp.StatusCode, data)
	apiErr.Method, apiErr.Path = method, pathOf(endpoint)
	apiErr.Code = redactText(apiErr.Code, c.token)
	apiErr.Message = redactText(apiErr.Message, c.token)
	return nil, retryAfter(resp.Header.Get("Retry-After")), apiErr
}

func pathOf(endpoint string) string {
	if i := strings.Index(endpoint, "/api/"); i >= 0 {
		return endpoint[i:]
	}
	return endpoint
}

func decodeAPIError(status int, data []byte) *APIError {
	var body struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Description      string `json:"description"`
	}
	_ = json.Unmarshal(data, &body)
	e := &APIError{Status: status, Code: body.Error}
	e.Message = body.ErrorDescription
	if e.Message == "" {
		e.Message = body.Description
	}
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	return e
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if isDigits(value) {
		// Cap before multiplying: a huge count would overflow the duration
		// into a negative or tiny wait.
		digits := strings.TrimLeft(value, "0")
		if len(digits) > 9 {
			return maxRetryWait
		}
		seconds, _ := strconv.Atoi(digits)
		d := time.Duration(seconds) * time.Second
		if d > maxRetryWait {
			return maxRetryWait
		}
		return d
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		if d > maxRetryWait {
			return maxRetryWait
		}
		return d
	}
	return 0
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Client) backoffFor(attempt int, wait time.Duration) time.Duration {
	if wait > 0 {
		return wait
	}
	d := c.backoff << attempt
	if d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

func retryableTransport(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return true
	}
	// A connection reset or an abrupt EOF is worth one more try; the request
	// is a read or an idempotent field write.
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection reset") || strings.Contains(text, "broken pipe") || strings.Contains(text, "unexpected eof")
}

// redact removes the bearer token from anything that might be rendered. The
// token is only ever placed in a header, but a transport error can echo a
// request representation.
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	text := strings.ReplaceAll(err.Error(), token, "[redacted]")
	return errors.New(text)
}

func redactText(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

func apiStatus(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out, true)
}

// post retries 5xx because its callers are idempotent field updates.
func (c *Client) post(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.do(ctx, http.MethodPost, path, query, body, out, true)
}

// postOnce does not retry 5xx: create and comment calls may already have
// committed before the response was lost, and a retry would duplicate them.
// A 429 is still retried because the request was not processed.
func (c *Client) postOnce(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.do(ctx, http.MethodPost, path, query, body, out, false)
}

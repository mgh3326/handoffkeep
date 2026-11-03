package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrLinearRateLimited is returned without a request while the shared Linear
// budget is paused after a 429.
var ErrLinearRateLimited = errors.New("linear api rate limited")

const (
	linearRatePauseDefault = time.Minute
	linearRatePauseMax     = time.Hour
)

// LinearRateGate is shared by every caller of one Linear credential. After a
// 429 it stops all requests until Linear's reset time, so a backlog cannot
// spend the hourly budget on requests that are certain to be refused.
type LinearRateGate struct {
	mu    sync.Mutex
	until time.Time
	now   func() time.Time
}

func (g *LinearRateGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// Paused reports whether requests must wait; a nil gate never pauses.
func (g *LinearRateGate) Paused() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clock().Before(g.until)
}

// limited records a 429 and returns the time requests resume.
func (g *LinearRateGate) limited(h http.Header) time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	// Retry-After is the per-request backoff; the requests reset is when the
	// hourly budget refills. Wait for whichever is later.
	var until time.Time
	retry := strings.TrimSpace(h.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(retry, 10, 64); err == nil && seconds >= 0 {
		until = now.Add(time.Duration(min(seconds, int64(linearRatePauseMax/time.Second))) * time.Second)
	} else if date, err := http.ParseTime(retry); err == nil {
		until = date
	}
	if ms, err := strconv.ParseInt(h.Get("X-RateLimit-Requests-Reset"), 10, 64); err == nil && ms > 0 && time.UnixMilli(ms).After(until) {
		until = time.UnixMilli(ms)
	}
	if until.IsZero() {
		until = now.Add(linearRatePauseDefault)
	}
	if floor := now.Add(time.Second); until.Before(floor) {
		until = floor
	}
	if ceiling := now.Add(linearRatePauseMax); until.After(ceiling) {
		until = ceiling
	}
	first := !now.Before(g.until)
	if until.After(g.until) {
		g.until = until
	}
	if first {
		log.Printf("linear_api.rate_limited resume_at=%s", g.until.UTC().Format(time.RFC3339))
	}
	return g.until
}

func (g *LinearRateGate) ResumeAt() time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until
}

type linearRateLimitError struct{ Until time.Time }

func (e linearRateLimitError) Error() string {
	return "linear rate limited until " + e.Until.UTC().Format(time.RFC3339)
}
func (g *LinearRateGate) rejection(h http.Header) error {
	until := time.Now().UTC().Add(linearRatePauseDefault)
	if g != nil {
		until = g.limited(h)
	}
	return linearRejectionError{err: linearRateLimitError{Until: until}, definite: true}
}

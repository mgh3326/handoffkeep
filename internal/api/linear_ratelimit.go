package api

import (
	"errors"
	"net/http"
	"strconv"
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
	until := now.Add(linearRatePauseDefault)
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		until = now.Add(time.Duration(s) * time.Second)
	} else if ms, err := strconv.ParseInt(h.Get("X-RateLimit-Requests-Reset"), 10, 64); err == nil && ms > 0 {
		until = time.UnixMilli(ms)
	}
	if floor := now.Add(time.Second); until.Before(floor) {
		until = floor
	}
	if ceiling := now.Add(linearRatePauseMax); until.After(ceiling) {
		until = ceiling
	}
	if until.After(g.until) {
		g.until = until
	}
	return g.until
}

package api

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One instance is shared by the board and its Linear HTTP client. This is
// worker flow control, not a metadata cache: metadata lives in hk's database.
type LinearBoardControl struct {
	mu           sync.Mutex
	missingLabel string
	missingTeam  string
	pausedUntil  time.Time
	rateFailures int
	calls        uint64
	now          func() time.Time
}

func (c *LinearBoardControl) time() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}
func (c *LinearBoardControl) blocked() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missingLabel != "" || c.time().Before(c.pausedUntil)
}
func (c *LinearBoardControl) rateError() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.time().Before(c.pausedUntil) {
		return linearRateLimitError{Until: c.pausedUntil}
	}
	return nil
}
func (c *LinearBoardControl) countCall() {
	if c != nil {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
	}
}
func (c *LinearBoardControl) CallCount() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
func (c *LinearBoardControl) breakLabel(name string, team ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.missingLabel == "" {
		c.missingLabel = name
		if len(team) > 0 {
			c.missingTeam = team[0]
		}
		log.Printf("linear_board.circuit_open reason=label_unavailable label=%q", name)
	}
}
func (c *LinearBoardControl) metadataChanged() {
	if c != nil {
		c.mu.Lock()
		c.missingLabel = ""
		c.missingTeam = ""
		c.mu.Unlock()
	}
}

type linearRateLimitError struct{ Until time.Time }

func (e linearRateLimitError) Error() string {
	return "linear rate limited until " + e.Until.UTC().Format(time.RFC3339)
}

func (c *LinearBoardControl) rateLimited(header string) error {
	now := c.time()
	delay := time.Minute
	if c != nil {
		c.mu.Lock()
		c.rateFailures++
		delay = min(time.Hour, time.Minute*time.Duration(1<<min(c.rateFailures-1, 6)))
		c.mu.Unlock()
	}
	until := now.Add(delay)
	if seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil && seconds >= 0 && seconds <= int64(time.Duration(1<<63-1)/time.Second) {
		until = now.Add(max(time.Second, time.Duration(seconds)*time.Second))
	} else if date, err := http.ParseTime(header); err == nil && date.After(now) {
		until = date
	}
	if c != nil {
		c.mu.Lock()
		if c.pausedUntil.After(until) {
			until = c.pausedUntil
		}
		first := !now.Before(c.pausedUntil)
		c.pausedUntil = until
		c.mu.Unlock()
		if first {
			log.Printf("linear_board.paused reason=rate_limit until=%q", until.Format(time.RFC3339))
		}
	}
	return linearRejectionError{err: linearRateLimitError{Until: until}}
}

type linearBoardLabelUnavailableError struct{ Name, TeamID string }

func (e linearBoardLabelUnavailableError) Error() string {
	return fmt.Sprintf("linear board label %q unavailable", e.Name)
}

func (c *LinearBoardControl) requestSucceeded() {
	if c != nil {
		c.mu.Lock()
		c.rateFailures = 0
		c.mu.Unlock()
	}
}

func (c *LinearBoardControl) missingLabelKey() (string, string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missingTeam, c.missingLabel
}
func (c *LinearBoardControl) labelResolved(team, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.missingLabel == name && c.missingTeam == team {
		c.missingLabel = ""
		c.missingTeam = ""
	}
}

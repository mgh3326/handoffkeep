package api

import (
	"fmt"
	"log"
	"sync"
)

// One instance is shared by the board and its Linear HTTP client. This is
// worker flow control, not a metadata cache: metadata lives in hk's database.
type LinearBoardControl struct {
	mu           sync.Mutex
	missingLabel string
	missingTeam  string
	calls        uint64
}

func (c *LinearBoardControl) blocked() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missingLabel != ""
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

type linearBoardLabelUnavailableError struct{ Name, TeamID string }

func (e linearBoardLabelUnavailableError) Error() string {
	return fmt.Sprintf("linear board label %q unavailable", e.Name)
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

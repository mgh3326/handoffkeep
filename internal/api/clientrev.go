package api

import (
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
)

// clientUserAgentPrefix is how remote.UserAgent starts: "handoffkeep/<rev>".
const clientUserAgentPrefix = "handoffkeep/"

// ClientRevisions remembers, per authenticated client, the build revision
// its CLI last reported in User-Agent. It is process memory only: a restart
// forgets it, and the next request from each client fills it again. The map
// is bounded by the configured token set because only authenticated client
// ids are keys. A nil *ClientRevisions records nothing.
type ClientRevisions struct {
	mu   sync.Mutex
	seen map[string]clientRevision
}

type clientRevision struct {
	rev  string
	seen time.Time
}

func NewClientRevisions() *ClientRevisions {
	return &ClientRevisions{seen: map[string]clientRevision{}}
}

// clientRevisionFromUserAgent returns the revision from a "handoffkeep/<rev>"
// User-Agent. rev must be "unknown" (an unstamped build) or a 7-64 character
// lowercase hex commit id; anything else is not an hk CLI and is ignored.
func clientRevisionFromUserAgent(userAgent string) (string, bool) {
	rev, ok := strings.CutPrefix(userAgent, clientUserAgentPrefix)
	if !ok {
		return "", false
	}
	if rev == "unknown" {
		return rev, true
	}
	if len(rev) < 7 || len(rev) > 64 {
		return "", false
	}
	for _, c := range rev {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return rev, true
}

// observe records the revision client sent and logs one line when it differs
// from the last one this process saw for that client (including the first).
func (c *ClientRevisions) observe(client, userAgent string) {
	if c == nil {
		return
	}
	rev, ok := clientRevisionFromUserAgent(userAgent)
	if !ok {
		return
	}
	now := time.Now().UTC()
	c.mu.Lock()
	prev, had := c.seen[client]
	c.seen[client] = clientRevision{rev: rev, seen: now}
	c.mu.Unlock()
	if !had || prev.rev != rev {
		previous := "none"
		if had {
			previous = prev.rev
		}
		log.Printf("api.client_revision client=%q rev=%s prev=%s", client, rev, previous)
	}
}

// writeMetrics appends one info series per client, Prometheus text format:
//
//	handoffkeep_client_revision_info{client="node",rev="<sha>"} 1
//	handoffkeep_client_last_seen_seconds{client="node"} <unix seconds>
func (c *ClientRevisions) writeMetrics(w io.Writer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	clients := make([]string, 0, len(c.seen))
	for client := range c.seen {
		clients = append(clients, client)
	}
	slices.Sort(clients)
	rows := make([]clientRevision, len(clients))
	for i, client := range clients {
		rows[i] = c.seen[client]
	}
	c.mu.Unlock()
	for i, client := range clients {
		label := promLabel(client)
		fmt.Fprintf(w, "handoffkeep_client_revision_info{client=%s,rev=%s} 1\n", label, promLabel(rows[i].rev))
		fmt.Fprintf(w, "handoffkeep_client_last_seen_seconds{client=%s} %d\n", label, rows[i].seen.Unix())
	}
}

// promLabel quotes a Prometheus label value: backslash, double quote and
// newline are the only escapes the text format defines.
func promLabel(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}

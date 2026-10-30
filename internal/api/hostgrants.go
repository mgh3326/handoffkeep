package api

import (
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
)

// hostGrantPrefix starts an auth-file line that lists the host ids a client
// may act for: HANDOFFKEEP_HOSTS_<CLIENT>=<id>[,<id>], the same shape as
// HANDOFFKEEP_QUOTA_<CLIENT>.
const hostGrantPrefix = "HANDOFFKEEP_HOSTS_"

// hostIDRE is the fleet host id alphabet: the node's HUB_MACHINE_ID.
var hostIDRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// HostGrants maps an authenticated client id to the host ids it may act for.
// A client absent from the map has no HANDOFFKEEP_HOSTS_ line. Every route
// that writes on behalf of a host asks Allows, so presence PUTs and later job
// writes share one parser and one rule.
type HostGrants map[string]map[string]bool

// Allows reports whether client may act for host. listed is false when the
// client has no host line at all; allowed is then true and the caller keeps
// its pre-grant behavior (see warnNoHostLine).
func (g HostGrants) Allows(client, host string) (allowed, listed bool) {
	hosts, listed := g[client]
	if !listed {
		return true, false
	}
	return hosts[host], true
}

// LoadHostGrants reads the HANDOFFKEEP_HOSTS_<CLIENT> lines of the auth file.
// Like quota grants, a bad line is logged and skipped, never a startup error,
// so an auth-file typo cannot stop the server. Unlike a quota grant, a
// skipped line fails closed: its client still counts as having a line, with
// whatever hosts its good lines list (none, if all are bad). Several lines
// for one client add up. A line with no client id, or for a client without a
// token, is dropped. Token and quota lines are LoadTokens'
// business and are ignored here.
func LoadHostGrants(path string, tokens Tokens) (HostGrants, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseHostGrants(string(b), tokens), nil
}

func parseHostGrants(data string, tokens Tokens) HostGrants {
	grants := HostGrants{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		id, isHosts := strings.CutPrefix(k, hostGrantPrefix)
		if !isHosts {
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" {
			log.Printf("api.host_grants_skipped reason=invalid_line line=%q", line)
			continue
		}
		if _, known := tokens[id]; !known {
			log.Printf("api.host_grants_skipped reason=unknown_client client=%q", id)
			continue
		}
		// Any line for a known client puts it under host grants, even a bad
		// one: a typo in a line meant to restrict the client must not lift
		// the restriction. A bad line adds no hosts, so a client left with
		// only bad lines is refused every host until the line is fixed.
		set := grants[id]
		if set == nil {
			set = map[string]bool{}
			grants[id] = set
		}
		hosts, valid := parseHostList(v)
		if !ok || !valid {
			log.Printf("api.host_grants_skipped reason=invalid_line client=%q line=%q", id, line)
			continue
		}
		for _, host := range hosts {
			set[host] = true
		}
	}
	return grants
}

// parseHostList splits a comma-separated list of host ids. Any empty or
// malformed entry makes the whole list invalid.
func parseHostList(v string) ([]string, bool) {
	var hosts []string
	for _, host := range strings.Split(v, ",") {
		host = strings.TrimSpace(host)
		if !hostIDRE.MatchString(host) {
			return nil, false
		}
		hosts = append(hosts, host)
	}
	return hosts, true
}

// noHostLineWarned holds the clients already warned about, per route, so a
// client that publishes every minute logs once per process.
var noHostLineWarned sync.Map

// warnNoHostLine logs that client wrote for host on route without a
// HANDOFFKEEP_HOSTS_ line. The write is accepted; enforcing it for clients
// without a line is a separate step once every client has one.
func warnNoHostLine(client, route, host string) {
	if _, seen := noHostLineWarned.LoadOrStore(client+"\x00"+route, true); seen {
		return
	}
	log.Printf("api.host_grants_missing client=%s route=%s host=%s", client, route, host)
}

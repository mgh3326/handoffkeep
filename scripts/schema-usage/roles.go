package main

import (
	"fmt"
	"strings"
)

// role is one value of the fixed role set every table takes as a label. The
// set is defined here once; the document's legend is generated from it.
type role struct {
	Name string
	Desc string
}

var roles = []role{
	{"source-of-record", "the authoritative copy of a fact; other tables may copy it"},
	{"history-log", "an append-only record of what happened, kept for audit or replay"},
	{"queue-outbox", "work waiting to be sent or processed; rows leave when done"},
	{"cache-derived", "a copy that can be rebuilt from a source-of-record table"},
	{"lock-lease", "a claim held by one owner until it is released or expires"},
	{"cursor-checkpoint", "a position in a feed or stream, so work resumes where it stopped"},
	{"link-join", "a relation between two entities, held as its own row"},
	{"config", "settings or reference data that the code reads and an operator sets"},
}

var roleSet = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range roles {
		m[r.Name] = true
	}
	return m
}()

func roleNames() []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

// roleLegend is the role set as the document states it, once, above the tables.
func roleLegend() string {
	var b strings.Builder
	b.WriteString("**Roles.** Every table carries one role label, from this fixed set:\n\n")
	for _, r := range roles {
		fmt.Fprintf(&b, "- `%s`: %s.\n", r.Name, r.Desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

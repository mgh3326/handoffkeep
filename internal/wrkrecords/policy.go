package wrkrecords

import (
	"encoding/json"
	"os"
)

// Policy has no exported mutable fields. It is a validated startup snapshot.
type Policy struct {
	digest      string
	clients     map[string]policyClient
	lanes       map[string]*string
	createLanes bool
}
type policyClient struct {
	Host        *string  `json:"host_client_id"`
	Permissions []string `json:"permissions"`
}

func LoadPolicy(file string) (*Policy, error) {
	b, e := os.ReadFile(file)
	if e != nil {
		return nil, Error("wrk_unavailable")
	}
	return ParsePolicy(b)
}
func ParsePolicy(b []byte) (*Policy, error) {
	v, e := Decode(b)
	if e != nil {
		return nil, Error("wrk_unavailable")
	}
	if Validate("Policy", v) != nil {
		return nil, Error("wrk_unavailable")
	}
	var wire struct {
		Clients  map[string]policyClient `json:"clients"`
		Lanes    map[string]*string      `json:"lanes"`
		Features struct {
			Create bool `json:"claim_lane_creation_enabled"`
		} `json:"features"`
	}
	if json.Unmarshal(b, &wire) != nil {
		return nil, Error("wrk_unavailable")
	}
	if parent, ok := wire.Lanes["director-1"]; !ok || parent != nil {
		return nil, Error("wrk_unavailable")
	}
	for lane := range wire.Lanes {
		seen := map[string]bool{}
		current := lane
		for {
			if seen[current] {
				return nil, Error("wrk_unavailable")
			}
			seen[current] = true
			p, ok := wire.Lanes[current]
			if !ok {
				return nil, Error("wrk_unavailable")
			}
			if p == nil {
				break
			}
			current = *p
		}
	}
	d, e := Digest(v)
	if e != nil {
		return nil, Error("wrk_unavailable")
	}
	return &Policy{digest: d, clients: wire.Clients, lanes: wire.Lanes, createLanes: wire.Features.Create}, nil
}
func (p *Policy) Digest() string {
	if p == nil {
		return ""
	}
	return p.digest
}
func (p *Policy) Allows(actor, permission string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.clients[actor].Permissions {
		if s == permission {
			return true
		}
	}
	return false
}
func (p *Policy) Host(actor string) string {
	if p == nil {
		return ""
	}
	h := p.clients[actor].Host
	if h == nil {
		return ""
	}
	return *h
}
func Central(actor string) bool { return actor == "director-1" || actor == "operator-desk" }

// ValidateGraph checks a replacement against the canonical graph while the
// store holds the global lane lock. Review assignments are routing pairs, not
// graph edges. Ended claims retain their historical edge but no live reference.
func (p *Policy) ValidateGraph(s Snapshot, previous *Policy) error {
	graph := map[string]string{}
	for lane, parent := range p.lanes {
		graph[lane] = ""
		if parent != nil {
			graph[lane] = *parent
		}
	}
	for job := range s.Jobs {
		c := s.Claim(job)
		if c == nil || Text(c.Payload, "role") != "builder" {
			continue
		}
		owner, parent := Text(c.Payload, "owner_lane"), Text(c.Payload, "parent_lane")
		if prior, ok := graph[owner]; ok && prior != parent {
			return Error("claim_conflict")
		}
		graph[owner] = parent
	}
	for job, rs := range s.Jobs {
		c := s.Claim(job)
		if c == nil {
			continue
		}
		ended := false
		for _, r := range rs {
			if r.Kind == "end" {
				ended = true
			}
		}
		if ended {
			continue
		}
		for _, lane := range []string{Text(c.Payload, "owner_lane"), Text(c.Payload, "parent_lane")} {
			if _, ok := graph[lane]; !ok {
				return Error("claim_conflict")
			}
			if previous != nil {
				if old, ok := previous.lanes[lane]; ok {
					next, exists := p.lanes[lane]
					if !exists || (old == nil) != (next == nil) || (old != nil && next != nil && *old != *next) {
						return Error("claim_conflict")
					}
				}
			}
		}
		if !Review(c) && graph[Text(c.Payload, "owner_lane")] != Text(c.Payload, "parent_lane") {
			return Error("claim_conflict")
		}
	}
	for lane := range graph {
		seen := map[string]bool{}
		for cur := lane; cur != ""; cur = graph[cur] {
			if seen[cur] {
				return Error("claim_conflict")
			}
			seen[cur] = true
			if _, ok := graph[cur]; !ok {
				return Error("claim_conflict")
			}
		}
	}
	return nil
}

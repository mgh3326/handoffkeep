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

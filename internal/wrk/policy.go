package wrk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sort"
)

// Permission names of the design §5 permission-to-operation map.
const (
	PermCapabilitiesRead   = "capabilities.read"
	PermJobRead            = "job.read"
	PermRecordsRead        = "records.read"
	PermContentRead        = "content.read"
	PermClaimWrite         = "claim.write"
	PermReviewerClaimWrite = "reviewer.claim.write"
	PermActivateWrite      = "activate.write"
	PermRoundOpen          = "round.open"
	PermVerdictWrite       = "verdict.write"
	PermVoidWrite          = "void.write"
	PermJoinedWrite        = "joined.write"
	PermCompletedWrite     = "completed.write"
	PermGrantWrite         = "grant.write"
	PermGrantCancel        = "grant.cancel"
	PermEndWrite           = "end.write"
	PermImportBegin        = "import.begin"
	PermImportSeal         = "import.seal"
	PermImportWrite        = "import.write"
	PermRelayPublish       = "relay.publish"
	PermTaskState          = "task.state"
)

// HostClientIDs are the five host ids; their policy entry maps each to
// itself (design §5 fixed ceilings).
var HostClientIDs = []string{"desktop", "mac-dev", "mac-work", "rpi", "mac-personal"}

// ClientIDs is the closed list of eleven policy clients: the original eight
// plus the three Q4 attribution ids.
var ClientIDs = []string{"desktop", "mac-dev", "mac-work", "rpi", "mac-personal", "director-1", "pa-integrator", "operator-desk", "at-integrator", "pb-integrator", "tt-integrator"}

// Policy is an immutable validated snapshot of the nonsecret server policy
// file. It never holds token material.
type Policy struct {
	sha     string
	clients map[string]policyClient
	lanes   map[string]*string
	// ClaimLaneCreation mirrors features.claim_lane_creation_enabled.
	ClaimLaneCreation bool
}

type policyClient struct {
	host  string
	perms map[string]bool
}

// ErrInvalidPolicy is returned for every policy that fails validation. It
// never echoes the file's contents.
var ErrInvalidPolicy = errors.New("invalid wrk policy")

// LoadPolicy reads and validates the policy file at path.
func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrInvalidPolicy
	}
	return ParsePolicy(b)
}

// ParsePolicy validates raw policy JSON against $defs.Policy (closed client
// list, per-client permission ceilings, host mapping and head_repeat_enabled
// const true) plus the §5 lane graph rules, and computes its digest.
func ParsePolicy(b []byte) (*Policy, error) {
	v, err := DecodeStrict(b)
	if err != nil {
		return nil, ErrInvalidPolicy
	}
	if err := ValidateDef("Policy", v); err != nil {
		return nil, ErrInvalidPolicy
	}
	canon, err := Canonical(v)
	if err != nil {
		return nil, ErrInvalidPolicy
	}
	sum := sha256.Sum256(canon)
	doc := v.(map[string]any)
	p := &Policy{sha: hex.EncodeToString(sum[:]), clients: map[string]policyClient{}, lanes: map[string]*string{}}
	for id, raw := range doc["clients"].(map[string]any) {
		entry := raw.(map[string]any)
		c := policyClient{perms: map[string]bool{}}
		if h, ok := entry["host_client_id"].(string); ok {
			c.host = h
		}
		for _, perm := range entry["permissions"].([]any) {
			c.perms[perm.(string)] = true
		}
		p.clients[id] = c
	}
	for lane, parent := range doc["lanes"].(map[string]any) {
		if s, ok := parent.(string); ok {
			p.lanes[lane] = &s
		} else {
			p.lanes[lane] = nil
		}
	}
	p.ClaimLaneCreation = doc["features"].(map[string]any)["claim_lane_creation_enabled"].(bool)
	if err := p.checkLanes(); err != nil {
		return nil, err
	}
	return p, nil
}

// checkLanes enforces the §5 lane map: director-1 is a root, the minimum
// pa-integrator edge is present, every parent is configured and there is no
// cycle.
func (p *Policy) checkLanes() error {
	if parent, ok := p.lanes["director-1"]; !ok || parent != nil {
		return ErrInvalidPolicy
	}
	if parent, ok := p.lanes["pa-integrator"]; !ok || parent == nil || *parent != "director-1" {
		return ErrInvalidPolicy
	}
	for lane := range p.lanes {
		seen := map[string]bool{}
		cur := lane
		for {
			if seen[cur] {
				return ErrInvalidPolicy
			}
			seen[cur] = true
			parent, ok := p.lanes[cur]
			if !ok {
				return ErrInvalidPolicy
			}
			if parent == nil {
				break
			}
			cur = *parent
		}
	}
	return nil
}

// SHA256 is the lowercase digest of the RFC 8785 form of the whole policy.
func (p *Policy) SHA256() string { return p.sha }

// Known reports whether id is one of the eleven policy clients.
func (p *Policy) Known(id string) bool {
	_, ok := p.clients[id]
	return ok
}

// Has reports whether client id holds perm. Unknown ids hold nothing.
func (p *Policy) Has(id, perm string) bool {
	return p != nil && p.clients[id].perms[perm]
}

// Host is the client's host_client_id ("" for the central and reader ids).
func (p *Policy) Host(id string) string { return p.clients[id].host }

// Central reports a director-1/operator-desk style client: no host mapping
// and the central claim permission.
func (p *Policy) Central(id string) bool {
	c, ok := p.clients[id]
	return ok && c.host == "" && c.perms[PermClaimWrite]
}

// LaneParent returns a configured lane's parent; ok is false when the lane
// is not in the policy map.
func (p *Policy) LaneParent(lane string) (parent *string, ok bool) {
	parent, ok = p.lanes[lane]
	return parent, ok
}

// Lanes lists the configured lane names in sorted order.
func (p *Policy) Lanes() []string {
	out := make([]string, 0, len(p.lanes))
	for l := range p.lanes {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Principal is one authenticated caller resolved against the policy. It is
// carried from the bearer-auth layer to the service and store fences; a
// caller-supplied "by" string never becomes a principal.
type Principal struct {
	ClientID string
	Policy   *Policy
}

// Has reports a permission of the principal.
func (pr Principal) Has(perm string) bool { return pr.Policy.Has(pr.ClientID, perm) }

// Host is the principal's host id ("" for non-host ids).
func (pr Principal) Host() string {
	if pr.Policy == nil {
		return ""
	}
	return pr.Policy.Host(pr.ClientID)
}

package ytwrite

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The YouTrack surface the issue-link projection reads and mutates (hk 3841,
// hk 4636 S1; design/2026-11-13/yt-issue-links §5.4, §6). Three primitives, no
// protocol: which links to add or remove, in which order, and what hk owns are
// the planner's (S3). Nothing here registers a Kind.

// LinkKind is a YouTrack link type hk projects. `blocks` is not one.
type LinkKind string

const (
	// LinkSubtask is the Subtask type; the writer is the child, so its link
	// target is the issue's parent.
	LinkSubtask LinkKind = "subtask"
	// LinkRelates is the undirected Relates type.
	LinkRelates LinkKind = "relates"
	// LinkDuplicate is the Duplicate type; the writer is the duplicate, so its
	// link target is the original it duplicates.
	LinkDuplicate LinkKind = "duplicate"
)

var linkKinds = []LinkKind{LinkSubtask, LinkRelates, LinkDuplicate}

// LinkDirection is the `direction` of a YouTrack issue link row.
type LinkDirection string

const (
	LinkOutward LinkDirection = "OUTWARD"
	LinkInward  LinkDirection = "INWARD"
	LinkBoth    LinkDirection = "BOTH"
)

// IssueLink is one link of the issue a read or mutation is about, seen from
// the side hk writes: for LinkSubtask the target is the issue's parent, for
// LinkDuplicate the original the issue duplicates, for LinkRelates the other
// end. Links the issue has from the other side (its children, the issues that
// duplicate it) are not IssueLinks.
type IssueLink struct {
	Kind   LinkKind
	Target string
}

// SortLinks orders links by kind (subtask, relates, duplicate), then target.
func SortLinks(links []IssueLink) {
	rank := map[LinkKind]int{LinkSubtask: 0, LinkRelates: 1, LinkDuplicate: 2}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Kind != links[j].Kind {
			return rank[links[i].Kind] < rank[links[j].Kind]
		}
		return links[i].Target < links[j].Target
	})
}

// RemoteLinkType is one link type as the instance lists it.
type RemoteLinkType struct {
	ID             string
	Name           string
	Directed       bool
	SourceToTarget string // the outward role name
	TargetToSource string // the inward role name
}

// ManagedLinkType is how one instance spells a managed kind: its type id and
// the direction of the row on which the issue plays the writer's role.
type ManagedLinkType struct {
	ID        string
	Direction LinkDirection
}

// Side is the suffix the type id takes in a /links/{linkID} URL: "s" for the
// outward side, "t" for the inward side, none for an undirected type.
func (t ManagedLinkType) Side() string {
	switch t.Direction {
	case LinkOutward:
		return "s"
	case LinkInward:
		return "t"
	}
	return ""
}

// LinkID is the {linkID} URL segment of the writer's side.
func (t ManagedLinkType) LinkID() string { return t.ID + t.Side() }

// LinkTypes are the three managed kinds on one instance.
type LinkTypes struct {
	Subtask   ManagedLinkType
	Relates   ManagedLinkType
	Duplicate ManagedLinkType
}

// Of is the managed type of kind; ok is false for a kind hk does not manage.
func (t LinkTypes) Of(kind LinkKind) (ManagedLinkType, bool) {
	switch kind {
	case LinkSubtask:
		return t.Subtask, t.Subtask.ID != ""
	case LinkRelates:
		return t.Relates, t.Relates.ID != ""
	case LinkDuplicate:
		return t.Duplicate, t.Duplicate.ID != ""
	}
	return ManagedLinkType{}, false
}

// ResolveLinkTypes picks the Subtask, Relates and Duplicate types out of the
// instance's list and the side of each the writer sits on. The side comes from
// the role names (`subtask of`, `duplicates`), never from a default: sides are
// instance configuration. A missing type, a type of the wrong directedness, or
// a directed type with none (or both) of the role names is a *RefusedError
// (ReasonFieldRefused); it never guesses.
func ResolveLinkTypes(listed []RemoteLinkType) (LinkTypes, error) {
	var out LinkTypes
	for _, spec := range []struct {
		name string
		role string // "" for the undirected type
		into *ManagedLinkType
	}{
		{"Subtask", "subtask of", &out.Subtask},
		{"Relates", "", &out.Relates},
		{"Duplicate", "duplicates", &out.Duplicate},
	} {
		var found []RemoteLinkType
		for _, typ := range listed {
			if strings.EqualFold(strings.TrimSpace(typ.Name), spec.name) {
				found = append(found, typ)
			}
		}
		if len(found) != 1 {
			return LinkTypes{}, refuseLinkType("YouTrack has %d link types named %q, want 1", len(found), spec.name)
		}
		typ := found[0]
		if typ.ID == "" {
			return LinkTypes{}, refuseLinkType("YouTrack link type %q has no id", spec.name)
		}
		if spec.role == "" {
			if typ.Directed {
				return LinkTypes{}, refuseLinkType("YouTrack link type %q is directed, want undirected", spec.name)
			}
			*spec.into = ManagedLinkType{ID: typ.ID, Direction: LinkBoth}
			continue
		}
		if !typ.Directed {
			return LinkTypes{}, refuseLinkType("YouTrack link type %q is undirected, want directed", spec.name)
		}
		inward, outward := sameRole(typ.TargetToSource, spec.role), sameRole(typ.SourceToTarget, spec.role)
		switch {
		case inward && !outward:
			*spec.into = ManagedLinkType{ID: typ.ID, Direction: LinkInward}
		case outward && !inward:
			*spec.into = ManagedLinkType{ID: typ.ID, Direction: LinkOutward}
		default:
			return LinkTypes{}, refuseLinkType("YouTrack link type %q has no single %q role (outward %q, inward %q)", spec.name, spec.role, typ.SourceToTarget, typ.TargetToSource)
		}
	}
	return out, nil
}

func sameRole(name, role string) bool { return strings.EqualFold(strings.TrimSpace(name), role) }

func refuseLinkType(format string, args ...any) error {
	return &RefusedError{Reason: ReasonFieldRefused, Detail: fmt.Sprintf(format, args...)}
}

// Classify says what a link row of type typeID and direction dir is. kind is
// "" for a type hk does not manage. ours is true when the row is on the side
// hk writes from this issue (the issue is the child, the duplicate, or an end
// of a relates link); a directed row on the other side (the issue's children,
// the issues that duplicate it) is the kind with ours false. A directed row
// whose direction is neither side is an error: the direction field is never
// guessed. An undirected type's rows are ours whatever their direction says.
func (t LinkTypes) Classify(typeID string, dir LinkDirection) (kind LinkKind, ours bool, err error) {
	for _, k := range linkKinds {
		managed, ok := t.Of(k)
		if !ok || managed.ID != typeID {
			continue
		}
		switch {
		case k == LinkRelates, dir == managed.Direction:
			return k, true, nil
		case dir == LinkInward || dir == LinkOutward:
			return k, false, nil
		}
		return k, false, fmt.Errorf("%w: a %s link row has direction %q", ErrInvalid, k, dir)
	}
	return "", false, nil
}

// MaxEnumeratedLinks bounds one read of one issue's link rows, and of its
// linked issues. More is ErrEnumerationTooLarge, never a partial result.
const MaxEnumeratedLinks = 10000

// IssueLinksRead is what one read of an issue's links saw: the instance's
// managed link types and every link on the managed side, sorted, distinct. The
// links are a list: an issue with two parents shows both.
type IssueLinksRead struct {
	Types LinkTypes
	Links []IssueLink
}

// IssueLinksRemote is the YouTrack surface of the issue-link projection. A
// Remote that does not implement it cannot send links. Each call is one
// request or, for a read, one complete paged enumeration; no retry inside, and
// a mutation never repeats. A non-2xx answer is a *RemoteError with the
// server's RetryAfter; a type the instance does not configure as hk needs is a
// *RefusedError (ReasonFieldRefused) before any link is read or written.
type IssueLinksRemote interface {
	// ReadIssueLinks reads the link types and every link row of the issue. A
	// result it cannot prove complete is an error (ErrEnumerationTooLarge).
	ReadIssueLinks(ctx context.Context, issueID string) (IssueLinksRead, error)
	// AddIssueLink adds one link from the issue's writer side. types is the
	// Types of a read.
	AddIssueLink(ctx context.Context, issueID string, types LinkTypes, link IssueLink) error
	// RemoveIssueLink removes one link from the issue's writer side. A link
	// that is already gone is not an error: the adapter re-reads when the
	// server says it is absent, and a read it cannot make is the error.
	RemoveIssueLink(ctx context.Context, issueID string, types LinkTypes, link IssueLink) error
}

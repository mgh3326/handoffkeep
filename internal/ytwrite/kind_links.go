package ytwrite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// The issue_links kind (hk 3841, hk 4638 S3; design/2026-11-13/yt-issue-links
// §5): the links one writer task wants on its own bound issue. The op's lane
// is the task ("task", decimal id), its target (YTTargetID) the task's bound
// issue. Its payload is the desired set only (I4): what to add and remove is
// planned at send time from a fresh read and the ledger's record of which
// links hk created (PlannedKind, links_plan.go), never from a baseline stored
// in the op. The store registers the kind's admission gate as FALSE until the
// consumer and the mode land (S4), so nothing sends it yet.

func init() { registerKind(KindIssueLinks, func(Config) Kind { return issueLinks{} }) }

// KindIssueLinks projects a task's parent, related and duplicate_of edges to
// its bound issue as YouTrack links.
const KindIssueLinks = "issue_links"

// The hk edge kinds a desired or held set names (the store's parent,
// related and duplicate_of).
const (
	LinkEdgeParent      = "parent"
	LinkEdgeRelated     = "related"
	LinkEdgeDuplicateOf = "duplicate_of"
)

// LinkKindOfEdge is the YouTrack link kind an hk edge kind projects to; ok is
// false for an edge kind that is not projected.
func LinkKindOfEdge(edge string) (LinkKind, bool) {
	switch edge {
	case LinkEdgeParent:
		return LinkSubtask, true
	case LinkEdgeRelated:
		return LinkRelates, true
	case LinkEdgeDuplicateOf:
		return LinkDuplicate, true
	}
	return "", false
}

// issueLinksPayloadVersion is the payload's "v". Version 1 was PR #387's
// payload with a `previous` baseline, which this kind refuses.
const issueLinksPayloadVersion = 2

// IssueLinkTarget is one link a writer wants: the hk task at the other end
// and that task's bound issue on the op's tenant.
type IssueLinkTarget struct {
	TaskID  int64  `json:"task_id"`
	IssueID string `json:"issue"`
}

// IssueLinkHeld is an edge of the writer whose target task has no binding on
// the op's tenant: neither written nor removed (I14).
type IssueLinkHeld struct {
	Kind   string `json:"kind"`
	TaskID int64  `json:"task_id"`
}

// IssueLinksDesired is the writer's desired set: its parent, the related
// edges it writes (it is the lower id) and its duplicate_of edges, each
// sorted by issue id.
type IssueLinksDesired struct {
	Parent     *IssueLinkTarget  `json:"parent,omitempty"`
	Related    []IssueLinkTarget `json:"related"`
	Duplicates []IssueLinkTarget `json:"duplicates"`
}

// IssueLinksSnapshot is the immutable payload of an issue_links op (v2): the
// desired set and the held edges, nothing else (I4).
type IssueLinksSnapshot struct {
	V       int               `json:"v"`
	Desired IssueLinksDesired `json:"desired"`
	Held    []IssueLinkHeld   `json:"held"`
}

// Links lists the desired set as links seen from the writer's issue, with
// the hk edge kind and target task of each, sorted (SortLinks order).
func (s IssueLinksSnapshot) Links() []DesiredLink {
	out := []DesiredLink{}
	if p := s.Desired.Parent; p != nil {
		out = append(out, DesiredLink{IssueLink{LinkSubtask, p.IssueID}, LinkEdgeParent, p.TaskID})
	}
	for _, r := range s.Desired.Related {
		out = append(out, DesiredLink{IssueLink{LinkRelates, r.IssueID}, LinkEdgeRelated, r.TaskID})
	}
	for _, d := range s.Desired.Duplicates {
		out = append(out, DesiredLink{IssueLink{LinkDuplicate, d.IssueID}, LinkEdgeDuplicateOf, d.TaskID})
	}
	sort.SliceStable(out, func(i, j int) bool { return linkLess(out[i].Link, out[j].Link) })
	return out
}

// DesiredLink is one desired link with the edge that wants it.
type DesiredLink struct {
	Link   IssueLink
	Edge   string
	TaskID int64
}

// IssueLinksRequest is one writer task's desired set for its bound issue.
type IssueLinksRequest struct {
	Principal string
	Actor     string
	// Tenant and IssueID are the writer task's binding.
	Tenant  string
	IssueID string
	TaskID  int64
	Desired IssueLinksDesired
	Held    []IssueLinkHeld
}

// CheckIssueLinks explains why a links request cannot be stored.
func CheckIssueLinks(req IssueLinksRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.TaskID <= 0 {
		return fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.IssueID) {
		return fmt.Errorf("%w: youtrack issue id is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	return checkIssueLinksSnapshot(req.TaskID, req.IssueID, IssueLinksSnapshot{V: issueLinksPayloadVersion, Desired: req.Desired, Held: req.Held})
}

// checkIssueLinksSnapshot checks a desired set against its writer: every
// target a positive task other than the writer with a well-formed issue other
// than the writer's, no issue twice in one kind, no task twice in one edge
// kind (desired or held), and at most one parent.
func checkIssueLinksSnapshot(writer int64, issue string, s IssueLinksSnapshot) error {
	if s.V != issueLinksPayloadVersion {
		return fmt.Errorf("%w: issue links payload version %d, want %d", ErrInvalid, s.V, issueLinksPayloadVersion)
	}
	tasks := map[[2]string]bool{}
	seenTask := func(edge string, id int64) error {
		k := [2]string{edge, strconv.FormatInt(id, 10)}
		if tasks[k] {
			return fmt.Errorf("%w: task %d is named twice as a %s target", ErrInvalid, id, edge)
		}
		tasks[k] = true
		return nil
	}
	issues := map[IssueLink]bool{}
	for _, d := range s.Links() {
		if d.TaskID <= 0 || d.TaskID == writer {
			return fmt.Errorf("%w: link target task %d is not positive or is the writer", ErrInvalid, d.TaskID)
		}
		if !issueIDRE.MatchString(d.Link.Target) || d.Link.Target == issue {
			return fmt.Errorf("%w: link target issue is empty, malformed, or the writer's own", ErrInvalid)
		}
		if issues[d.Link] {
			return fmt.Errorf("%w: %s link to %s is named twice", ErrInvalid, d.Link.Kind, d.Link.Target)
		}
		issues[d.Link] = true
		if err := seenTask(d.Edge, d.TaskID); err != nil {
			return err
		}
	}
	parents := 0
	if s.Desired.Parent != nil {
		parents++
	}
	for _, h := range s.Held {
		if _, ok := LinkKindOfEdge(h.Kind); !ok {
			return fmt.Errorf("%w: held edge kind %q is not projected", ErrInvalid, h.Kind)
		}
		if h.TaskID <= 0 || h.TaskID == writer {
			return fmt.Errorf("%w: held task %d is not positive or is the writer", ErrInvalid, h.TaskID)
		}
		if err := seenTask(h.Kind, h.TaskID); err != nil {
			return err
		}
		if h.Kind == LinkEdgeParent {
			parents++
		}
	}
	if parents > 1 {
		return fmt.Errorf("%w: more than one parent", ErrInvalid)
	}
	return nil
}

// canonical returns s with every list sorted and non-nil, so one desired set
// has one payload and one PayloadSHA.
func (s IssueLinksSnapshot) canonical() IssueLinksSnapshot {
	byIssue := func(in []IssueLinkTarget) []IssueLinkTarget {
		out := append([]IssueLinkTarget{}, in...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].IssueID != out[j].IssueID {
				return out[i].IssueID < out[j].IssueID
			}
			return out[i].TaskID < out[j].TaskID
		})
		return out
	}
	out := IssueLinksSnapshot{V: s.V, Desired: IssueLinksDesired{
		Related:    byIssue(s.Desired.Related),
		Duplicates: byIssue(s.Desired.Duplicates),
	}}
	if p := s.Desired.Parent; p != nil {
		cp := *p
		out.Desired.Parent = &cp
	}
	out.Held = append([]IssueLinkHeld{}, s.Held...)
	sort.Slice(out.Held, func(i, j int) bool {
		if out.Held[i].Kind != out.Held[j].Kind {
			return out.Held[i].Kind < out.Held[j].Kind
		}
		return out.Held[i].TaskID < out.Held[j].TaskID
	})
	return out
}

// IssueLinksOp is the op a links request stores, with no Key: the store's
// snapshot helper sets it from PayloadSHA, which covers the kind, the task,
// the issue, the tenant, the actor and the canonical payload.
func IssueLinksOp(req IssueLinksRequest) (NewOp, error) {
	if err := CheckIssueLinks(req); err != nil {
		return NewOp{}, err
	}
	snap := IssueLinksSnapshot{V: issueLinksPayloadVersion, Desired: req.Desired, Held: req.Held}.canonical()
	payload, err := json.Marshal(snap)
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.TaskID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindIssueLinks,
		PayloadSHA: payloadSHA(KindIssueLinks, "task", entityID, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.IssueID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// IssueLinksPayload decodes and checks an issue_links op's payload. A payload
// with a field v2 does not name (PR #387's `previous` baseline among them) is
// ErrInvalid, as is an op that is not on a task lane.
func IssueLinksPayload(op Op) (IssueLinksSnapshot, error) {
	writer, ok := positiveDecimal(op.EntityID)
	if op.Kind != KindIssueLinks || op.EntityKind != "task" || !ok || !issueIDRE.MatchString(op.YTTargetID) || op.Tenant == "" {
		return IssueLinksSnapshot{}, fmt.Errorf("%w: op %d is not a task issue-links snapshot", ErrInvalid, op.ID)
	}
	var s IssueLinksSnapshot
	dec := json.NewDecoder(bytes.NewReader(op.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return IssueLinksSnapshot{}, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if dec.More() {
		return IssueLinksSnapshot{}, fmt.Errorf("%w: payload of op %d has trailing data", ErrInvalid, op.ID)
	}
	if err := checkIssueLinksSnapshot(writer, op.YTTargetID, s); err != nil {
		return IssueLinksSnapshot{}, err
	}
	return s, nil
}

// LinkWriter is the writer task of an issue_links op (its lane's task id).
func LinkWriter(op Op) (int64, error) {
	id, ok := positiveDecimal(op.EntityID)
	if op.EntityKind != "task" || !ok {
		return 0, fmt.Errorf("%w: op %d is not on a task lane", ErrInvalid, op.ID)
	}
	return id, nil
}

// issueLinks is the issue_links kind. It is a PlannedKind: the protocol
// sends and reconciles it through Read, the ledger's plan, Execute and
// Converged, and never through Send or Evidence.
type issueLinks struct{}

var _ PlannedKind = issueLinks{}

// Render is the canonical payload: the op is never sent as text.
func (issueLinks) Render(op Op) (string, error) {
	if _, err := IssueLinksPayload(op); err != nil {
		return "", err
	}
	return string(op.Payload), nil
}

func (issueLinks) Send(context.Context, Remote, Op, string) (string, error) {
	return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "issue_links is sent by its plan, not by Send"}
}

func (issueLinks) Evidence(context.Context, Remote, Op, string) (Evidence, error) {
	return Evidence{}, errors.New("issue_links is recovered by its plan, not by an evidence search")
}

// MapRows: a link creates no hk entity to map.
func (issueLinks) MapRows(Op, string) []MapRow { return nil }

// Read reads the bound issue's links. A remote that cannot read links, or a
// payload the kind refuses, is a *RefusedError: nothing is read or written.
func (issueLinks) Read(ctx context.Context, remote Remote, op Op) (IssueLinksRead, error) {
	lr, ok := remote.(IssueLinksRemote)
	if !ok {
		return IssueLinksRead{}, &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot read or write issue links"}
	}
	if _, err := IssueLinksPayload(op); err != nil {
		return IssueLinksRead{}, &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	return lr.ReadIssueLinks(ctx, op.YTTargetID)
}

// Execute sends the plan's diff, one request per link, in the order I12
// fixes: subtask removes, subtask adds, the other adds, the other removes, so
// a re-parent drops the old parent before it adds the new one. Before each
// request it calls mark with the step's index; a mark that fails stops it
// with nothing more sent. It stops at the first error and returns how many
// mutations it completed; an error after one is a *PartialWriteError.
func (issueLinks) Execute(ctx context.Context, remote Remote, op Op, types LinkTypes, plan LinkPlan, mark func(int) error) (int, error) {
	lr, ok := remote.(IssueLinksRemote)
	if !ok {
		return 0, &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot read or write issue links"}
	}
	steps := ExecuteOrder(plan)
	for i, s := range steps {
		err := mark(i)
		if err == nil {
			if s.Add {
				err = lr.AddIssueLink(ctx, op.YTTargetID, types, s.Link)
			} else {
				err = lr.RemoveIssueLink(ctx, op.YTTargetID, types, s.Link)
			}
		}
		if err != nil {
			if i > 0 {
				return i, &PartialWriteError{Err: err}
			}
			return 0, err
		}
	}
	return len(steps), nil
}

// Converged reports whether a read shows the plan done: every desired link
// the plan did not leave to another writer is present, and no link the plan
// removes is. Unowned and peer links do not count (I7).
func (issueLinks) Converged(op Op, plan LinkPlan, cur []IssueLink) (bool, error) {
	s, err := IssueLinksPayload(op)
	if err != nil {
		return false, err
	}
	return linksConverged(s, plan, cur), nil
}

func linksConverged(s IssueLinksSnapshot, plan LinkPlan, cur []IssueLink) bool {
	have := linkSet(cur)
	peer := linkSet(plan.Peer)
	for _, d := range s.Links() {
		if !have[d.Link] && !peer[d.Link] {
			return false
		}
	}
	for _, l := range plan.Remove {
		if have[l] {
			return false
		}
	}
	return true
}

// LinkStep is one mutation of a plan.
type LinkStep struct {
	Add  bool
	Link IssueLink
}

// ExecuteOrder is the order Execute sends a plan's mutations in (I12).
func ExecuteOrder(plan LinkPlan) []LinkStep {
	var out []LinkStep
	pick := func(links []IssueLink, add, subtask bool) {
		for _, l := range links {
			if (l.Kind == LinkSubtask) == subtask {
				out = append(out, LinkStep{Add: add, Link: l})
			}
		}
	}
	pick(plan.Remove, false, true)
	pick(plan.Add, true, true)
	pick(plan.Add, true, false)
	pick(plan.Remove, false, false)
	return out
}

// PartialWriteError is a multi-request write that completed at least one
// mutation before a later one failed: YouTrack's state changed, so the op is
// remote_unknown whatever the failing status was.
type PartialWriteError struct {
	Err error
}

func (e *PartialWriteError) Error() string { return "partial write: " + e.Err.Error() }
func (e *PartialWriteError) Unwrap() error { return e.Err }

func linkSet(links []IssueLink) map[IssueLink]bool {
	out := make(map[IssueLink]bool, len(links))
	for _, l := range links {
		out[l] = true
	}
	return out
}

func linkLess(a, b IssueLink) bool {
	rank := map[LinkKind]int{LinkSubtask: 0, LinkRelates: 1, LinkDuplicate: 2}
	if a.Kind != b.Kind {
		return rank[a.Kind] < rank[b.Kind]
	}
	return a.Target < b.Target
}

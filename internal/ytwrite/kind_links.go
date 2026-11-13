package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// KindIssueLinks writes the complete hk-owned link snapshot for a task issue.
const KindIssueLinks = "issue_links"

const ReasonUnownedLink = "unowned_link"

func init() { registerKind(KindIssueLinks, func(Config) Kind { return issueLinks{} }) }

// IssueLinks is the hk-owned subset of one issue's links. Targets are YouTrack
// issue ids resolved from hk task bindings before the operation is prepared.
type IssueLinks struct {
	Parent     string   `json:"parent,omitempty"`
	Related    []string `json:"related"`
	Duplicates []string `json:"duplicates"`
}

// IssueLinksRequest is one link snapshot for a bound task issue.
type IssueLinksRequest struct {
	Principal string
	Actor     string
	Tenant    string
	IssueID   string
	TaskID    int64
	Previous  IssueLinks
	Links     IssueLinks
}

// IssueLinksSnapshot carries the current desired set and the last set hk
// successfully projected. Only links in Previous may be removed; other
// existing YouTrack links are preserved.
type IssueLinksSnapshot struct {
	Previous IssueLinks `json:"previous"`
	Desired  IssueLinks `json:"desired"`
}

// CheckIssueLinks validates a task issue link snapshot.
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
	for _, id := range append(append([]string{req.Links.Parent}, req.Links.Related...), req.Links.Duplicates...) {
		if id != "" && (!issueIDRE.MatchString(id) || id == req.IssueID) {
			return fmt.Errorf("%w: link target is empty, malformed, or self-referential", ErrInvalid)
		}
	}
	return nil
}

// IssueLinksOp builds the immutable task-lane snapshot op.
func IssueLinksOp(req IssueLinksRequest) (NewOp, error) {
	if err := CheckIssueLinks(req); err != nil {
		return NewOp{}, err
	}
	if err := CheckIssueLinks(IssueLinksRequest{Principal: req.Principal, Actor: req.Actor, Tenant: req.Tenant,
		IssueID: req.IssueID, TaskID: req.TaskID, Links: req.Previous}); err != nil {
		return NewOp{}, err
	}
	req.Links.Related = append([]string(nil), req.Links.Related...)
	req.Links.Duplicates = append([]string(nil), req.Links.Duplicates...)
	sort.Strings(req.Links.Related)
	sort.Strings(req.Links.Duplicates)
	req.Previous.Related = sortedLinks(req.Previous.Related)
	req.Previous.Duplicates = sortedLinks(req.Previous.Duplicates)
	payload, err := json.Marshal(IssueLinksSnapshot{Previous: req.Previous, Desired: req.Links})
	if err != nil {
		return NewOp{}, err
	}
	id := strconv.FormatInt(req.TaskID, 10)
	return NewOp{Principal: req.Principal, Kind: KindIssueLinks,
		PayloadSHA: payloadSHA(KindIssueLinks, "task", id, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload, EntityKind: "task", EntityID: id, YTTargetID: req.IssueID, Tenant: req.Tenant, Actor: req.Actor}, nil
}

// IssueLinksPayload returns the checked link snapshots of an issue_links op.
func IssueLinksPayload(op Op) (IssueLinksSnapshot, error) {
	var snapshot IssueLinksSnapshot
	if op.EntityKind != "task" || !issueIDRE.MatchString(op.YTTargetID) {
		return snapshot, fmt.Errorf("%w: op %d is not a task issue-links snapshot", ErrInvalid, op.ID)
	}
	var err error
	snapshot, err = DecodeIssueLinksSnapshot(op.Payload)
	if err != nil {
		return snapshot, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if err := CheckIssueLinks(IssueLinksRequest{Principal: op.Principal, Actor: op.Actor, Tenant: op.Tenant,
		IssueID: op.YTTargetID, TaskID: 1, Links: snapshot.Desired}); err != nil {
		return snapshot, err
	}
	if err := CheckIssueLinks(IssueLinksRequest{Principal: op.Principal, Actor: op.Actor, Tenant: op.Tenant,
		IssueID: op.YTTargetID, TaskID: 1, Links: snapshot.Previous}); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// DecodeIssueLinksSnapshot decodes the immutable link projection payload.
func DecodeIssueLinksSnapshot(payload []byte) (IssueLinksSnapshot, error) {
	var snapshot IssueLinksSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// IssueLinksRemote is the YouTrack surface of issue_links.
type IssueLinksRemote interface {
	UpdateIssueLinks(ctx context.Context, issueID string, snapshot IssueLinksSnapshot) (id string, err error)
	ReadIssueLinks(ctx context.Context, issueID string) (IssueLinks, error)
}

type issueLinks struct{}

func (issueLinks) Render(Op) (string, error) { return "", nil }

func (issueLinks) Send(ctx context.Context, remote Remote, op Op, _ string) (string, error) {
	r, ok := remote.(IssueLinksRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot update issue links"}
	}
	snapshot, err := IssueLinksPayload(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	return r.UpdateIssueLinks(ctx, op.YTTargetID, snapshot)
}

func (issueLinks) Evidence(ctx context.Context, remote Remote, op Op, _ string) (Evidence, error) {
	var ev Evidence
	r, ok := remote.(IssueLinksRemote)
	if !ok {
		return ev, errors.New("the remote cannot read issue links")
	}
	want, err := IssueLinksPayload(op)
	if err != nil {
		return ev, err
	}
	got, err := r.ReadIssueLinks(ctx, op.YTTargetID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	if issueLinksConverged(want, got) {
		ev.Matches = []string{op.YTTargetID}
	} else {
		ev.Mismatched = 1
	}
	return ev, nil
}

// Recover re-reads and diffs the remote link set before applying only the
// remaining per-link mutations. These operations are idempotent; recovery
// never replays a stored replacement request.
func (issueLinks) Recover(ctx context.Context, remote Remote, op Op) (string, error) {
	r, ok := remote.(IssueLinksRemote)
	if !ok {
		return "", errors.New("the remote cannot recover issue links")
	}
	snapshot, err := IssueLinksPayload(op)
	if err != nil {
		return "", err
	}
	return r.UpdateIssueLinks(ctx, op.YTTargetID, snapshot)
}

func issueLinksConverged(snapshot IssueLinksSnapshot, got IssueLinks) bool {
	return linkConverged(snapshot.Previous.Parent, snapshot.Desired.Parent, got.Parent) &&
		linkSetConverged(snapshot.Previous.Related, snapshot.Desired.Related, got.Related) &&
		linkSetConverged(snapshot.Previous.Duplicates, snapshot.Desired.Duplicates, got.Duplicates)
}

func linkConverged(previous, desired, got string) bool {
	return got == desired || (got != previous && desired == "")
}

func linkSetConverged(previous, desired, got []string) bool {
	for _, id := range desired {
		if !containsString(got, id) {
			return false
		}
	}
	for _, id := range previous {
		if !containsString(desired, id) && containsString(got, id) {
			return false
		}
	}
	return true
}

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func sortedLinks(ids []string) []string {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	if ids == nil {
		return []string{}
	}
	return ids
}

func (issueLinks) MapRows(Op, string) []MapRow { return nil }

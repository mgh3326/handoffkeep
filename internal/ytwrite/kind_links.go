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
	Links     IssueLinks
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
	req.Links.Related = append([]string(nil), req.Links.Related...)
	req.Links.Duplicates = append([]string(nil), req.Links.Duplicates...)
	sort.Strings(req.Links.Related)
	sort.Strings(req.Links.Duplicates)
	payload, err := json.Marshal(req.Links)
	if err != nil {
		return NewOp{}, err
	}
	id := strconv.FormatInt(req.TaskID, 10)
	return NewOp{Principal: req.Principal, Kind: KindIssueLinks,
		PayloadSHA: payloadSHA(KindIssueLinks, "task", id, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload, EntityKind: "task", EntityID: id, YTTargetID: req.IssueID, Tenant: req.Tenant, Actor: req.Actor}, nil
}

// IssueLinksPayload returns the checked desired links of an issue_links op.
func IssueLinksPayload(op Op) (IssueLinks, error) {
	var links IssueLinks
	if op.EntityKind != "task" || !issueIDRE.MatchString(op.YTTargetID) {
		return links, fmt.Errorf("%w: op %d is not a task issue-links snapshot", ErrInvalid, op.ID)
	}
	if err := json.Unmarshal(op.Payload, &links); err != nil {
		return links, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if err := CheckIssueLinks(IssueLinksRequest{Principal: op.Principal, Actor: op.Actor, Tenant: op.Tenant,
		IssueID: op.YTTargetID, TaskID: 1, Links: links}); err != nil {
		return links, err
	}
	return links, nil
}

// IssueLinksRemote is the YouTrack surface of issue_links.
type IssueLinksRemote interface {
	UpdateIssueLinks(ctx context.Context, issueID string, links IssueLinks) (id string, err error)
	ReadIssueLinks(ctx context.Context, issueID string) (IssueLinks, error)
}

type issueLinks struct{}

func (issueLinks) Render(Op) (string, error) { return "", nil }

func (issueLinks) Send(ctx context.Context, remote Remote, op Op, _ string) (string, error) {
	r, ok := remote.(IssueLinksRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot update issue links"}
	}
	links, err := IssueLinksPayload(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	return r.UpdateIssueLinks(ctx, op.YTTargetID, links)
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
	if issueLinksEqual(want, got) {
		ev.Matches = []string{op.YTTargetID}
	} else {
		ev.Mismatched = 1
	}
	return ev, nil
}

func issueLinksEqual(a, b IssueLinks) bool {
	return a.Parent == b.Parent && equalStrings(a.Related, b.Related) && equalStrings(a.Duplicates, b.Duplicates)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (issueLinks) MapRows(Op, string) []MapRow { return nil }

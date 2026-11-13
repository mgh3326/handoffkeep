package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueLinksRemote = (*WriteRemote)(nil)

// UpdateIssueLinks changes individual links after checking that every link it
// would remove appeared in the last successfully projected hk snapshot.
func (r *WriteRemote) UpdateIssueLinks(ctx context.Context, issueID string, snapshot ytwrite.IssueLinksSnapshot) (string, error) {
	types, err := r.c.ListLinkTypes(ctx)
	if err != nil {
		return "", &ytwrite.NotSentError{Err: err}
	}
	byName := map[string]LinkType{}
	for _, typ := range types {
		byName[strings.ToLower(typ.Name)] = typ
	}
	for _, want := range []struct {
		name     string
		directed bool
	}{{"subtask", true}, {"relates", false}, {"duplicate", true}} {
		typ, ok := byName[want.name]
		if !ok || typ.Directed != want.directed {
			return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonInvalidIssue, Detail: fmt.Sprintf("YouTrack link type %q is missing or has the wrong direction", want.name)}
		}
	}
	current, err := r.ReadIssueLinks(ctx, issueID)
	if err != nil {
		return "", &ytwrite.NotSentError{Err: err}
	}
	if detail := unexpectedIssueLink(current.Parent, snapshot.Previous.Parent, snapshot.Desired.Parent); detail != "" {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned parent link to " + detail}
	}
	if detail := unexpectedIssueLinkSet(current.Related, snapshot.Previous.Related, snapshot.Desired.Related); detail != "" {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned relates link to " + detail}
	}
	if detail := unexpectedIssueLinkSet(current.Duplicates, snapshot.Previous.Duplicates, snapshot.Desired.Duplicates); detail != "" {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned duplicate link to " + detail}
	}
	linkID := func(name, side string) string {
		typ := byName[name]
		if !typ.Directed {
			return typ.ID
		}
		return typ.ID + side
	}
	mutations := 0
	for _, change := range []struct {
		name     string
		side     string
		previous []string
		desired  []string
		current  []string
	}{
		{"subtask", "t", optionalTarget(snapshot.Previous.Parent), optionalTarget(snapshot.Desired.Parent), optionalTarget(current.Parent)},
		{"relates", "", snapshot.Previous.Related, snapshot.Desired.Related, current.Related},
		{"duplicate", "t", snapshot.Previous.Duplicates, snapshot.Desired.Duplicates, current.Duplicates},
	} {
		for _, target := range change.desired {
			if containsLink(change.current, target) {
				continue
			}
			if err := r.mutateIssueLink(ctx, http.MethodPost, issueID, linkID(change.name, change.side), target); err != nil {
				if mutations > 0 {
					return "", &ytwrite.PartialWriteError{Err: err}
				}
				return "", err
			}
			mutations++
		}
		for _, target := range change.previous {
			if containsLink(change.desired, target) || !containsLink(change.current, target) {
				continue
			}
			if err := r.mutateIssueLink(ctx, http.MethodDelete, issueID, linkID(change.name, change.side), target); err != nil {
				if mutations > 0 {
					return "", &ytwrite.PartialWriteError{Err: err}
				}
				return "", err
			}
			mutations++
		}
	}
	verified, err := r.ReadIssueLinks(ctx, issueID)
	if err != nil {
		return "", fmt.Errorf("link update verification uncertain: %v", err)
	}
	if !issueLinkSnapshotConverged(snapshot, verified) {
		return "", nil
	}
	return issueID, nil
}

func (r *WriteRemote) mutateIssueLink(ctx context.Context, method, issueID, linkID, target string) error {
	path := "/api/issues/" + url.PathEscape(issueID) + "/links/" + url.PathEscape(linkID) + "/issues/" + url.PathEscape(target)
	var body any
	if method == http.MethodPost {
		path = "/api/issues/" + url.PathEscape(issueID) + "/links/" + url.PathEscape(linkID) + "/issues"
		body = map[string]string{"id": target}
	}
	q := url.Values{}
	q.Set("fields", "id")
	_, _, err := r.send(ctx, method, path, q, body)
	return err
}

func optionalTarget(target string) []string {
	if target == "" {
		return []string{}
	}
	return []string{target}
}

func unexpectedIssueLink(current, previous, desired string) string {
	if current != "" && current != desired && current != previous {
		return current
	}
	return ""
}

func unexpectedIssueLinkSet(current, previous, desired []string) string {
	for _, id := range current {
		if !containsLink(desired, id) && !containsLink(previous, id) {
			return id
		}
	}
	return ""
}

func containsLink(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func issueLinkSnapshotConverged(snapshot ytwrite.IssueLinksSnapshot, got ytwrite.IssueLinks) bool {
	if snapshot.Desired.Parent != "" && got.Parent != snapshot.Desired.Parent {
		return false
	}
	if snapshot.Previous.Parent != "" && snapshot.Previous.Parent != snapshot.Desired.Parent && got.Parent == snapshot.Previous.Parent {
		return false
	}
	return issueLinkSetConverged(snapshot.Previous.Related, snapshot.Desired.Related, got.Related) &&
		issueLinkSetConverged(snapshot.Previous.Duplicates, snapshot.Desired.Duplicates, got.Duplicates)
}

func issueLinkSetConverged(previous, desired, got []string) bool {
	for _, id := range desired {
		if !containsLink(got, id) {
			return false
		}
	}
	for _, id := range previous {
		if !containsLink(desired, id) && containsLink(got, id) {
			return false
		}
	}
	return true
}

// ReadIssueLinks reads the currently configured hk-owned link directions.
func (r *WriteRemote) ReadIssueLinks(ctx context.Context, issueID string) (ytwrite.IssueLinks, error) {
	q := url.Values{}
	q.Set("fields", "id,direction,linkType(name,sourceToTarget,targetToSource),issues(id)")
	path := "/api/issues/" + url.PathEscape(issueID) + "/links"
	data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return ytwrite.IssueLinks{}, err
	}
	if readErr != nil {
		return ytwrite.IssueLinks{}, readErr
	}
	var rows []struct {
		ID        string `json:"id"`
		Direction string `json:"direction"`
		LinkType  struct {
			Name           string `json:"name"`
			SourceToTarget string `json:"sourceToTarget"`
			TargetToSource string `json:"targetToSource"`
		} `json:"linkType"`
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return ytwrite.IssueLinks{}, fmt.Errorf("youtrack GET %s: decode: %w", path, err)
	}
	out := ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}
	for _, row := range rows {
		for _, issue := range row.Issues {
			switch strings.ToLower(row.LinkType.Name) {
			case "subtask":
				if row.Direction == "INWARD" && row.LinkType.TargetToSource != "" {
					out.Parent = issue.ID
				}
			case "relates", "relate":
				out.Related = append(out.Related, issue.ID)
			case "duplicate":
				if row.Direction == "INWARD" && row.LinkType.TargetToSource != "" {
					out.Duplicates = append(out.Duplicates, issue.ID)
				}
			}
		}
	}
	sort.Strings(out.Related)
	sort.Strings(out.Duplicates)
	return out, nil
}

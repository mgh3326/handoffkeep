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

// UpdateIssueLinks replaces only the three link directions hk owns on one
// issue. YouTrack treats links in an issue update as a replacement per type
// direction, so empty issue lists remove stale hk-owned links.
func (r *WriteRemote) UpdateIssueLinks(ctx context.Context, issueID string, links ytwrite.IssueLinks) (string, error) {
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
	linkID := func(name, side string) string {
		typ := byName[name]
		if !typ.Directed {
			return typ.ID
		}
		return typ.ID + side
	}
	type linkPayload struct {
		ID     string `json:"id"`
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	makeLink := func(id string, targets []string) linkPayload {
		p := linkPayload{ID: id, Issues: make([]struct {
			ID string `json:"id"`
		}, 0, len(targets))}
		for _, target := range targets {
			p.Issues = append(p.Issues, struct {
				ID string `json:"id"`
			}{target})
		}
		return p
	}
	body := struct {
		Links []linkPayload `json:"links"`
	}{Links: []linkPayload{
		makeLink(linkID("subtask", "t"), optionalTarget(links.Parent)),
		makeLink(linkID("relates", ""), links.Related),
		makeLink(linkID("duplicate", "s"), links.Duplicates),
	}}
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(issueID), q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil || out.ID != issueID {
		return "", nil
	}
	return out.ID, nil
}

func optionalTarget(target string) []string {
	if target == "" {
		return []string{}
	}
	return []string{target}
}

// ReadIssueLinks reads the currently configured hk-owned link directions.
func (r *WriteRemote) ReadIssueLinks(ctx context.Context, issueID string) (ytwrite.IssueLinks, error) {
	q := url.Values{}
	q.Set("fields", "id,direction,linkType(name),issues(id)")
	path := "/api/issues/" + url.PathEscape(issueID) + "/links"
	data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return ytwrite.IssueLinks{}, err
	}
	if readErr != nil {
		return ytwrite.IssueLinks{}, readErr
	}
	var rows []struct {
		ID       string `json:"id"`
		LinkType struct {
			Name string `json:"name"`
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
				if strings.HasSuffix(row.ID, "t") {
					out.Parent = issue.ID
				}
			case "relates", "relate":
				out.Related = append(out.Related, issue.ID)
			case "duplicate":
				if strings.HasSuffix(row.ID, "s") {
					out.Duplicates = append(out.Duplicates, issue.ID)
				}
			}
		}
	}
	sort.Strings(out.Related)
	sort.Strings(out.Duplicates)
	return out, nil
}

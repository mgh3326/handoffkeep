package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueMoveRemote = (*WriteRemote)(nil)

// MoveIssueToProject makes one POST /api/issues/{id} that sets the issue's
// project (hk 4055, the issue_move kind). The reply's id is returned only when
// the reply is the same issue, in projectID; anything else is "" (uncertain),
// and the op is settled by reading the issue's project (IssueProjectID). A
// read failure of the reply is "" too: the move may have happened.
func (r *WriteRemote) MoveIssueToProject(ctx context.Context, issueID, projectID string) (string, error) {
	q := url.Values{}
	q.Set("fields", "id,project(id)")
	body := struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}{}
	body.Project.ID = projectID
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(issueID), q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", nil
	}
	var out struct {
		ID      string `json:"id"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if json.Unmarshal(data, &out) != nil || out.ID != issueID || out.Project.ID != projectID {
		return "", nil
	}
	return out.ID, nil
}

// IssueProjectID reads the issue's project id in one GET (hk 4055, the
// issue_move kind's evidence).
func (r *WriteRemote) IssueProjectID(ctx context.Context, issueID string) (string, error) {
	q := url.Values{}
	q.Set("fields", "id,project(id)")
	path := "/api/issues/" + url.PathEscape(issueID)
	data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", readErr
	}
	var out struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("youtrack GET %s: decode: %w", path, err)
	}
	return out.Project.ID, nil
}

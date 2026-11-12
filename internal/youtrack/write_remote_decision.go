package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.DecisionRemote = (*WriteRemote)(nil)

// CheckDecisionFields reads the DEC project's fields in one request and
// reports a missing hk ID or 결정 ID field, so decision_create holds its op
// instead of having the search or the create refused for good.
func (r *WriteRemote) CheckDecisionFields(ctx context.Context, projectID string) error {
	shapes, err := r.projectFieldShapes(ctx, projectID)
	if err != nil {
		return err
	}
	for _, name := range []string{ytwrite.FieldHKID, ytwrite.FieldDecisionID} {
		if _, ok := shapes[name]; !ok {
			return fmt.Errorf("youtrack project %s has no %q field", projectID, name)
		}
	}
	return nil
}

// FindDecisionIssues searches the DEC project for the issues whose 결정 ID is
// decisionID in one request (decision_create, hk 4054), checking each hit
// against the project id and the field value as FindTaskIssues does.
func (r *WriteRemote) FindDecisionIssues(ctx context.Context, projectShort, projectID string, decisionID int64) ([]ytwrite.RemoteIssue, error) {
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/issues", issueFieldQuery(projectShort, ytwrite.FieldDecisionID, decisionID, taskIssueSearchTop), nil)
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	var page []Issue
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("youtrack GET /api/issues: decode: %w", err)
	}
	if len(page) >= taskIssueSearchTop {
		return nil, fmt.Errorf("youtrack GET /api/issues: %d or more issues match 결정 ID %d; the search is not complete", taskIssueSearchTop, decisionID)
	}
	out := []ytwrite.RemoteIssue{}
	for _, issue := range page {
		if !issueHasField(issue, projectID, ytwrite.FieldDecisionID, decisionID) {
			continue
		}
		hit := ytwrite.RemoteIssue{ID: issue.ID, Readable: issue.IDReadable, Summary: issue.Summary, Description: issue.Description}
		if issue.Project != nil {
			hit.ProjectID = issue.Project.ID
		}
		out = append(out, hit)
	}
	return out, nil
}

// CreateDecisionIssue makes one POST /api/issues with the DEC project, the
// summary, the description and only the hk ID and 결정 ID fields: the state,
// lane, recommendation, due time and linked task are the decision_fields
// snapshot's, which keeps their usual 4xx causes (a missing bundle value) out
// of the create.
func (r *WriteRemote) CreateDecisionIssue(ctx context.Context, in ytwrite.DecisionIssueCreate) (string, error) {
	id := int(in.DecisionID)
	if int64(id) != in.DecisionID {
		return "", fmt.Errorf("decision id %d does not fit an int field", in.DecisionID)
	}
	body := issueBody{Summary: in.Summary, Description: in.Description, CustomFields: []CustomField{
		IntField(ytwrite.FieldHKID, id), IntField(ytwrite.FieldDecisionID, id),
	}}
	body.Project.ID = in.ProjectID
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues", q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		// The issue may exist; the id is unknown.
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", nil
	}
	return out.ID, nil
}

// DecisionReadable is the readable id the import gave a decision's issue:
// DEC-<decision id> (the import keys a DEC issue by the decision id).
func DecisionReadable(decisionID int64) string {
	return DecisionProject + "-" + strconv.FormatInt(decisionID, 10)
}

// VerifyDecisionIssue explains why an issue is not the DEC issue of decision
// decisionID ("" when it is): its readable id is wantReadable (the import's
// DEC-<id>; "" for the legacy mirror's issue, whose number YouTrack chose), it
// is in the DEC project (decProjectID), its summary names the decision
// ("D-<id>" or the legacy "[결정 <id>]", then a space or the end), and its
// 결정 ID field equals the id (hk 3833 §2.1). `youtrack decisions-backfill`
// binds only an issue this accepts.
func VerifyDecisionIssue(issue Issue, decisionID int64, decProjectID, wantReadable string) string {
	switch {
	case issue.ID == "":
		return "no issue id"
	case wantReadable != "" && issue.IDReadable != wantReadable:
		return fmt.Sprintf("readable id %q is not %s", issue.IDReadable, wantReadable)
	case issue.Project == nil || issue.Project.ID != decProjectID:
		return "not in the " + DecisionProject + " project"
	case !ytwrite.SummaryNamesDecision(issue.Summary, decisionID):
		return fmt.Sprintf("summary %q does not name decision %d", truncateRunes(issue.Summary, 80), decisionID)
	}
	f, ok := IssueFieldByName(issue.CustomFields, ytwrite.FieldDecisionID)
	if !ok {
		return "no " + ytwrite.FieldDecisionID + " field"
	}
	if v, ok := f.IntValue(); !ok || int64(v) != decisionID {
		return ytwrite.FieldDecisionID + " is not " + strconv.FormatInt(decisionID, 10)
	}
	return ""
}

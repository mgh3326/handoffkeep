package ytwritetest

import (
	"context"
	"fmt"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.DecisionRemote = (*FakeYouTrack)(nil)

// InjectDecisionIssue adds a DEC issue hk did not create (the import's, or
// an earlier one) to the project, carrying 결정 ID and hk ID decisionID with
// the readable id DEC-<decisionID>, and returns its id.
func (f *FakeYouTrack) InjectDecisionIssue(projectID string, decisionID int64, summary, description string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addDecisionIssueLocked(projectID, decisionID, summary, description, "DEC-"+strconv.FormatInt(decisionID, 10))
}

func (f *FakeYouTrack) addDecisionIssueLocked(projectID string, decisionID int64, summary, description, readable string) string {
	f.nextID++
	id := fmt.Sprintf("2-%d", f.nextID)
	if readable == "" {
		readable = fmt.Sprintf("DEC-%d", 10000+f.nextID)
	}
	i := &fakeIssue{
		RemoteIssue: ytwrite.RemoteIssue{ID: id, Readable: readable, ProjectID: projectID, Summary: summary, Description: description},
		hkID:        decisionID,
		decisionID:  decisionID,
	}
	setFakeField(i, ytwrite.FieldHKID, strconv.FormatInt(decisionID, 10))
	setFakeField(i, ytwrite.FieldDecisionID, strconv.FormatInt(decisionID, 10))
	f.projects[projectID] = append(f.projects[projectID], i)
	return id
}

// CheckDecisionFields refuses a project marked by SetIssueFieldsMissing.
func (f *FakeYouTrack) CheckDecisionFields(ctx context.Context, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.fieldsMissing[projectID] {
		return fmt.Errorf("project %s has no %s field", projectID, ytwrite.FieldDecisionID)
	}
	return nil
}

// FindDecisionIssues returns every issue of the project carrying 결정 ID
// decisionID, failing as QueueFind says. The short name is not checked: the
// fake keys issues by project id.
func (f *FakeYouTrack) FindDecisionIssues(ctx context.Context, _, projectID string, decisionID int64) ([]ytwrite.RemoteIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(f.findFaults) > 0 {
		err := f.findFaults[0]
		f.findFaults = f.findFaults[1:]
		return nil, err
	}
	out := []ytwrite.RemoteIssue{}
	for _, i := range f.projects[projectID] {
		if i.decisionID == decisionID {
			out = append(out, i.RemoteIssue)
		}
	}
	return out, nil
}

// CreateDecisionIssue creates one DEC issue with a new readable id (REST
// takes the project's next number, never DEC-<decision>), failing as the next
// queued Fault says.
func (f *FakeYouTrack) CreateDecisionIssue(ctx context.Context, in ytwrite.DecisionIssueCreate) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.decisionCreates = append(f.decisionCreates, in)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.createLocked(func() string {
		return f.addDecisionIssueLocked(in.ProjectID, in.DecisionID, in.Summary, in.Description, "")
	})
}

// DecisionCreates returns every DecisionIssueCreate the fake received, in
// order.
func (f *FakeYouTrack) DecisionCreates() []ytwrite.DecisionIssueCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ytwrite.DecisionIssueCreate(nil), f.decisionCreates...)
}

package ytwritetest

import (
	"context"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueMoveRemote = (*FakeYouTrack)(nil)

// FakeMove is one MoveIssueToProject call the fake received.
type FakeMove struct {
	IssueID   string
	ProjectID string
}

// Moves returns every move the fake received, in order.
func (f *FakeYouTrack) Moves() []FakeMove {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeMove(nil), f.moves...)
}

// ProjectOf returns the project the issue is in now, or false for an unknown
// issue.
func (f *FakeYouTrack) ProjectOf(issueID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.issueLocked(issueID)
	if i == nil {
		return "", false
	}
	return i.ProjectID, true
}

// moveLocked puts issue i in project projectID, as YouTrack does: the issue
// keeps its id and leaves its old project's listing.
func (f *FakeYouTrack) moveLocked(i *fakeIssue, projectID string) {
	old := f.projects[i.ProjectID]
	for n, other := range old {
		if other == i {
			f.projects[i.ProjectID] = append(old[:n:n], old[n+1:]...)
			break
		}
	}
	i.ProjectID = projectID
	f.projects[projectID] = append(f.projects[projectID], i)
}

// MoveIssueToProject moves the issue, failing as the next queued Fault says
// (as UpdateIssueFields does). An unknown issue is a 404.
func (f *FakeYouTrack) MoveIssueToProject(ctx context.Context, issueID, projectID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.moves = append(f.moves, FakeMove{IssueID: issueID, ProjectID: projectID})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var fault Fault
	if len(f.faults) > 0 {
		fault, f.faults = f.faults[0], f.faults[1:]
	}
	i := f.issueLocked(issueID)
	if i == nil {
		return "", &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "issue not found"}
	}
	commit := func() string {
		f.commits++
		f.moveLocked(i, projectID)
		return i.ID
	}
	switch fault.Kind {
	case CommitDropReply:
		commit()
		return "", errConnReset
	case NoID:
		commit()
		return "", nil
	case RateLimit:
		return "", &ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: fault.RetryAfter, Message: "rate limited"}
	case ServerError:
		if fault.Commit {
			commit()
		}
		st := fault.Status
		if st == 0 {
			st = http.StatusBadGateway
		}
		return "", &ytwrite.RemoteError{Status: st, Message: "server error"}
	case Refuse:
		return "", &ytwrite.RemoteError{Status: fault.Status, Message: "refused"}
	case DelayCommit:
		panic("ytwritetest: DelayCommit is not supported for issue moves")
	}
	return commit(), nil
}

// IssueProjectID returns the issue's project as it is now; an unknown issue
// is a 404. It takes the queued read faults, as ReadIssueFields does.
func (f *FakeYouTrack) IssueProjectID(ctx context.Context, issueID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(f.readFaults) > 0 {
		err := f.readFaults[0]
		f.readFaults = f.readFaults[1:]
		return "", err
	}
	i := f.issueLocked(issueID)
	if i == nil {
		return "", &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "issue not found"}
	}
	return i.ProjectID, nil
}

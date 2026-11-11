package youtrack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ErrCanaryRefused is a task whose YouTrack issue cannot be bound for the
// comment canary (slice b1a) or for comment mode on (hk 3810). The text after
// "canary_refused: " says why.
var ErrCanaryRefused = errors.New("canary_refused")

// ErrNoIssue is the refusals that mean the task has no issue to bind (no
// project, a project youtrack_projects does not map, no such issue). Every
// one also matches ErrCanaryRefused; any other refusal is a disagreement.
var ErrNoIssue = errors.New("no_issue")

// IssueReader reads one issue.
type IssueReader interface {
	GetIssue(ctx context.Context, issueID string) (Issue, error)
}

// VerifyCanary binds a task to its imported YouTrack issue: <short name>-<task
// id> in the project youtrack_projects names for the task's project (no OTHER
// fallback), whose readable id is exactly that and whose summary starts with
// "#<task id>" followed by a space or nothing. The old mirror link
// (task_origins) and a yt_entity_map row, when present, must name the same
// issue on the same tenant; the first plausible issue is never picked. The
// binding keeps the stable internal id, so a later rename or move does not
// change where the canary writes. A failed read is returned as it is (not a
// refusal): the operator can retry.
func VerifyCanary(ctx context.Context, issues IssueReader, tenant string, c store.YTCanaryCandidate) (store.YTCommentCanary, error) {
	refuse := func(format string, args ...any) (store.YTCommentCanary, error) {
		return store.YTCommentCanary{}, fmt.Errorf("%w: "+format, append([]any{ErrCanaryRefused}, args...)...)
	}
	noIssue := func(format string, args ...any) (store.YTCommentCanary, error) {
		return store.YTCommentCanary{}, fmt.Errorf("%w: %w: "+format, append([]any{ErrCanaryRefused, ErrNoIssue}, args...)...)
	}
	if c.TaskID <= 0 {
		return refuse("task id must be positive")
	}
	if c.Project == "" {
		return noIssue("task %d has no project", c.TaskID)
	}
	if c.ShortName == "" {
		return noIssue("project %q has no youtrack_projects row", c.Project)
	}
	id := strconv.FormatInt(c.TaskID, 10)
	readable := c.ShortName + "-" + id
	issue, err := issues.GetIssue(ctx, readable)
	if errors.Is(err, ErrNotFound) {
		return noIssue("issue %s does not exist", readable)
	}
	if err != nil {
		return store.YTCommentCanary{}, err
	}
	switch {
	case issue.ID == "":
		return refuse("issue %s answered without an id", readable)
	case issue.IDReadable != readable:
		return refuse("%s answers as %s (moved or renumbered)", readable, issue.IDReadable)
	case !ytwrite.SummaryNamesTask(issue.Summary, c.TaskID):
		return refuse("summary of %s does not start with #%s", readable, id)
	case issue.Project == nil || !strings.EqualFold(issue.Project.ShortName, c.ShortName) || (c.ProjectID != "" && issue.Project.ID != c.ProjectID):
		return refuse("%s is not in the mapped project %s", readable, c.ShortName)
	case c.MirrorIssueID != "" && (c.MirrorIssueID != issue.ID || (c.MirrorTenant != "" && c.MirrorTenant != tenant)):
		return refuse("task_origins links task %d to issue %s on %s, not %s", c.TaskID, c.MirrorIssueID, c.MirrorTenant, issue.ID)
	case c.MappedIssueID != "" && c.MappedIssueID != issue.ID:
		return refuse("yt_entity_map maps task %d to issue %s, not %s", c.TaskID, c.MappedIssueID, issue.ID)
	}
	return store.YTCommentCanary{TaskID: c.TaskID, Tenant: tenant, IssueID: issue.ID, IssueReadable: issue.IDReadable, ProjectID: issue.Project.ID}, nil
}

// VerifyBinding is VerifyCanary for comment mode on (hk 3810): the same
// checks, and the task's project must also have its YouTrack project id in
// youtrack_projects (a refusal that matches ErrNoIssue, made without a
// YouTrack call), because mode on compares the binding's project with that id
// at every comment. A failed read is returned as it is, not as a refusal.
func VerifyBinding(ctx context.Context, issues IssueReader, tenant string, c store.YTCanaryCandidate) (store.YTTaskBinding, error) {
	if c.Project != "" && c.ShortName != "" && c.ProjectID == "" {
		return store.YTTaskBinding{}, fmt.Errorf("%w: %w: project %q has no YouTrack project id in youtrack_projects", ErrCanaryRefused, ErrNoIssue, c.Project)
	}
	v, err := VerifyCanary(ctx, issues, tenant, c)
	if err != nil {
		return store.YTTaskBinding{}, err
	}
	return store.YTTaskBinding{TaskID: v.TaskID, Tenant: v.Tenant, IssueID: v.IssueID, IssueReadable: v.IssueReadable, ProjectID: v.ProjectID}, nil
}

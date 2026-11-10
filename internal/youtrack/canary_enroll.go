package youtrack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ErrCanaryRefused is a task whose YouTrack issue cannot be bound for the
// comment canary (slice b1a). The text after "canary_refused: " says why.
var ErrCanaryRefused = errors.New("canary_refused")

// ErrNoIssue is not implemented yet.
var ErrNoIssue = errors.New("no_issue")

// VerifyBinding is not implemented yet.
func VerifyBinding(ctx context.Context, issues IssueReader, tenant string, c store.YTCanaryCandidate) (store.YTTaskBinding, error) {
	return store.YTTaskBinding{}, errors.New("VerifyBinding: not implemented")
}

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
	if c.TaskID <= 0 {
		return refuse("task id must be positive")
	}
	if c.Project == "" {
		return refuse("task %d has no project", c.TaskID)
	}
	if c.ShortName == "" {
		return refuse("project %q has no youtrack_projects row", c.Project)
	}
	id := strconv.FormatInt(c.TaskID, 10)
	readable := c.ShortName + "-" + id
	issue, err := issues.GetIssue(ctx, readable)
	if errors.Is(err, ErrNotFound) {
		return refuse("issue %s does not exist", readable)
	}
	if err != nil {
		return store.YTCommentCanary{}, err
	}
	switch {
	case issue.ID == "":
		return refuse("issue %s answered without an id", readable)
	case issue.IDReadable != readable:
		return refuse("%s answers as %s (moved or renumbered)", readable, issue.IDReadable)
	case !SummaryNamesTask(issue.Summary, c.TaskID):
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

// SummaryNamesTask reports whether an imported issue summary names the task:
// "#<id>" then a space or the end. "#37" does not name task 3, and "#037" or
// "#3739" do not name task 37.
func SummaryNamesTask(summary string, taskID int64) bool {
	rest, ok := strings.CutPrefix(summary, "#"+strconv.FormatInt(taskID, 10))
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r)
}

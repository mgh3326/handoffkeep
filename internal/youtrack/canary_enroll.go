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
	return verifyIssue(ctx, issues, tenant, c, c.ShortName, c.ProjectID)
}

// verifyIssue checks the issue <shortName>-<task id>: it exists, answers to
// exactly that readable id, its summary names the task, it is in the project
// shortName (and projectID, when not ""), and the task's mirror link and map
// row, when present, name it on the same tenant.
func verifyIssue(ctx context.Context, issues IssueReader, tenant string, c store.YTCanaryCandidate, shortName, projectID string) (store.YTCommentCanary, error) {
	refuse := func(format string, args ...any) (store.YTCommentCanary, error) {
		return store.YTCommentCanary{}, fmt.Errorf("%w: "+format, append([]any{ErrCanaryRefused}, args...)...)
	}
	noIssue := func(format string, args ...any) (store.YTCommentCanary, error) {
		return store.YTCommentCanary{}, fmt.Errorf("%w: %w: "+format, append([]any{ErrCanaryRefused, ErrNoIssue}, args...)...)
	}
	if c.TaskID <= 0 {
		return refuse("task id must be positive")
	}
	id := strconv.FormatInt(c.TaskID, 10)
	readable := shortName + "-" + id
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
	case !SummaryNamesTask(issue.Summary, c.TaskID):
		return refuse("summary of %s does not start with #%s", readable, id)
	case issue.Project == nil || !strings.EqualFold(issue.Project.ShortName, shortName) || (projectID != "" && issue.Project.ID != projectID):
		return refuse("%s is not in the mapped project %s", readable, shortName)
	case c.MirrorIssueID != "" && (c.MirrorIssueID != issue.ID || (c.MirrorTenant != "" && c.MirrorTenant != tenant)):
		return refuse("task_origins links task %d to issue %s on %s, not %s", c.TaskID, c.MirrorIssueID, c.MirrorTenant, issue.ID)
	case c.MappedIssueID != "" && c.MappedIssueID != issue.ID:
		return refuse("yt_entity_map maps task %d to issue %s, not %s", c.TaskID, c.MappedIssueID, issue.ID)
	}
	return store.YTCommentCanary{TaskID: c.TaskID, Tenant: tenant, IssueID: issue.ID, IssueReadable: issue.IDReadable, ProjectID: issue.Project.ID}, nil
}

// BucketShortName is the short name of the YouTrack project the import puts
// every task with no hk project in (its /unassigned bucket): such a task's
// issue is HK-<task id>. hk keeps no youtrack_projects row for it
// (reservedShortNames).
const BucketShortName = "HK"

// ProjectLister lists every YouTrack project.
type ProjectLister interface {
	ListAdminProjects(ctx context.Context) ([]AdminProject, error)
}

// ErrBucketProject is a YouTrack project list with no project, or more than
// one, whose short name is BucketShortName.
var ErrBucketProject = errors.New("bucket_project")

// ResolveBucketProject returns the id of the one YouTrack project whose short
// name is BucketShortName (YouTrack compares short names without case). None,
// or more than one, is ErrBucketProject: the caller refuses its whole run
// rather than bind a task with no project to a guess. A failed read is
// returned as it is.
func ResolveBucketProject(ctx context.Context, projects ProjectLister) (string, error) {
	all, err := projects.ListAdminProjects(ctx)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, p := range all {
		if strings.EqualFold(p.ShortName, BucketShortName) {
			ids = append(ids, p.ID)
		}
	}
	switch {
	case len(ids) != 1:
		return "", fmt.Errorf("%w: %d YouTrack projects have short name %s %v, want exactly one", ErrBucketProject, len(ids), BucketShortName, ids)
	case ids[0] == "":
		return "", fmt.Errorf("%w: project %s answered without an id", ErrBucketProject, BucketShortName)
	}
	return ids[0], nil
}

// VerifyBinding is VerifyCanary for comment mode on (hk 3810): the same
// checks, and the task's project must also have its YouTrack project id in
// youtrack_projects (a refusal that matches ErrNoIssue, made without a
// YouTrack call), because mode on compares the binding's project with that id
// at every comment. A task with no project (hk 3897) is checked the same way
// against the import's bucket: issue HK-<task id> in the project bucketID
// (ResolveBucketProject); with bucketID "" it has no issue, as before. A
// failed read is returned as it is, not as a refusal.
func VerifyBinding(ctx context.Context, issues IssueReader, tenant, bucketID string, c store.YTCanaryCandidate) (store.YTTaskBinding, error) {
	if c.Project != "" && c.ShortName != "" && c.ProjectID == "" {
		return store.YTTaskBinding{}, fmt.Errorf("%w: %w: project %q has no YouTrack project id in youtrack_projects", ErrCanaryRefused, ErrNoIssue, c.Project)
	}
	var v store.YTCommentCanary
	var err error
	if c.Project == "" && bucketID != "" {
		v, err = verifyIssue(ctx, issues, tenant, c, BucketShortName, bucketID)
	} else {
		v, err = VerifyCanary(ctx, issues, tenant, c)
	}
	if err != nil {
		return store.YTTaskBinding{}, err
	}
	return store.YTTaskBinding{TaskID: v.TaskID, Tenant: v.Tenant, IssueID: v.IssueID, IssueReadable: v.IssueReadable, ProjectID: v.ProjectID}, nil
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

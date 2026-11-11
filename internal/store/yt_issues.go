package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Issue mode (hk 3898, docs/youtrack-write-protocol.md "Kind issue_create"):
// hk creates a task's YouTrack issue itself. CreateTask prepares one
// issue_create op in the task's own transaction, `youtrack issues-backfill`
// prepares one for every unbound task, and the write worker sends it. When the
// op is applied, the same transaction publishes the task's binding
// (yt_task_bindings, origin op_applied), so later comment ops have a target.

// YTIssuesMode gates issue creation (HANDOFFKEEP_YT_ISSUES_MODE).
type YTIssuesMode string

const (
	// YTIssuesOff prepares no issue op and admits none (the default).
	YTIssuesOff YTIssuesMode = "off"
	// YTIssuesOn prepares an issue_create op for each new task whose
	// project maps to a YouTrack project, and admits issue_create ops.
	YTIssuesOn YTIssuesMode = "on"
)

// YTIssuePrincipal owns the issue_create ops; their key is
// ytwrite.IssueKey(task id), so a task has at most one.
const YTIssuePrincipal = "hk:task-issues"

type ytIssuesGate struct {
	mode   YTIssuesMode
	tenant string
}

// SetYTIssuesMode sets the gate for this process and the YouTrack tenant a new
// task's op is bound to. serve calls it once at start, before any request. On
// with an empty tenant is off.
func (s *Store) SetYTIssuesMode(m YTIssuesMode, tenant string) {
	s.ytIssues.Store(&ytIssuesGate{mode: m, tenant: tenant})
}

// ytIssuesNow is the gate: on and its tenant, or off.
func (s *Store) ytIssuesNow() (YTIssuesMode, string) {
	g := s.ytIssues.Load()
	if g == nil || g.mode != YTIssuesOn || g.tenant == "" {
		return YTIssuesOff, ""
	}
	return YTIssuesOn, g.tenant
}

func init() { registerYTKindGate(ytwrite.KindIssueCreate, ytIssueGateSQL) }

// ytIssueGateSQL is the issue_create gate (ytAdmitSQL): mode on, and o's task
// has no binding or a binding this op published. A binding from the import
// (or another op) holds the op: the task already has its issue. Mode off
// admits nothing.
func ytIssueGateSQL(s *Store) string {
	if mode, _ := s.ytIssuesNow(); mode != YTIssuesOn {
		return `FALSE`
	}
	return `o.entity_kind = 'task' AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.op_id IS DISTINCT FROM o.id)`
}

// ytIssueProject is a task's hk project and the YouTrack project it maps to.
type ytIssueProject struct {
	project, shortName, projectID string
}

// readYTIssueProject reads the task's youtrack_projects row. found is false
// when the task has no project, its project has no row, or the row has no
// YouTrack project id yet: there is no project to create the issue in.
func readYTIssueProject(ctx context.Context, q ytRowQuerier, project string) (ytIssueProject, bool, error) {
	p := ytIssueProject{project: project}
	if project == "" {
		return p, false, nil
	}
	err := q.QueryRow(ctx, `SELECT coalesce(short_name,''), coalesce(yt_project_id,'') FROM youtrack_projects WHERE hk_project=$1`, project).Scan(&p.shortName, &p.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	return p, p.projectID != "" && p.shortName != "", nil
}

// prepareYTIssueTx stores task's issue_create op in tx, bound to tenant, at
// now. A task the protocol cannot render gets a rejected op (invalid_issue)
// and nothing is ever sent. created is false when the task already had its op
// (the key replays). The caller has read p.
func prepareYTIssueTx(ctx context.Context, tx pgx.Tx, now time.Time, t Task, p ytIssueProject, tenant string) (ytwrite.Op, bool, error) {
	req := ytwrite.IssueRequest{Principal: YTIssuePrincipal, Actor: t.CreatedBy, TaskID: t.ID, Title: t.Title,
		Project: p.project, YTProjectID: p.projectID, YTProjectShort: p.shortName, Tenant: tenant}
	n, err := ytwrite.IssueOp(req)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	op, created, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil || !created {
		return op, created, err
	}
	if cerr := ytwrite.CheckIssue(req); cerr != nil {
		op, err = rejectYTOpTx(ctx, tx, now, op.ID, ytwrite.ReasonInvalidIssue, cerr.Error())
	}
	return op, true, err
}

// enqueueYTIssueTx is CreateTask's step in mode on, after the task insert and
// before its commit: the task's issue_create op, or nothing (one log line)
// when its project maps to no YouTrack project. Any database error fails the
// whole create.
//
// Locks, after CreateTask's own (lockorder.go): a plain read of the task's
// youtrack_projects row, then the new task's lane lock and its new
// yt_write_ops row (prepareYTOpTx). Nothing else can hold a brand-new task's
// lane.
func (s *Store) enqueueYTIssueTx(ctx context.Context, tx pgx.Tx, t Task) error {
	mode, tenant := s.ytIssuesNow()
	if mode != YTIssuesOn {
		return nil
	}
	p, found, err := readYTIssueProject(ctx, tx, deref(t.Project))
	if err != nil {
		return err
	}
	if !found {
		log.Printf("ytwrite.issue_skipped task_id=%d project=%q reason=project_unmapped", t.ID, deref(t.Project))
		return nil
	}
	op, _, err := prepareYTIssueTx(ctx, tx, t.CreatedAt.UTC(), t, p, tenant)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.issue_enqueued op_id=%d task_id=%d state=%s reason=%q", op.ID, t.ID, op.State, op.Reason)
	return nil
}

// bindYTIssueTx publishes the binding of an issue_create op being applied
// with issue ytID, in Apply's transaction (which holds the task's lane
// lock). It never overwrites: a binding of the task to the same issue on the
// same tenant is kept as it is; a binding of the task to another issue, or of
// the issue to another task, keeps the op applied, inserts nothing and
// returns a non-empty conflict detail (logged as ytwrite.binding_conflict).
// The readable id is not known here (the protocol carries only the stable
// id), so it is left empty.
func bindYTIssueTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time) (string, error) {
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || taskID < 1 || op.EntityKind != "task" {
		return "", fmt.Errorf("issue_create op %d is not on a task lane (%s:%s)", op.ID, op.EntityKind, op.EntityID)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, origin, op_id)
		VALUES ($1,$2,$3,'',$4,$5,$6,'op_applied',$7) ON CONFLICT DO NOTHING`,
		taskID, op.Tenant, ytID, op.YTTargetID, op.Principal, now.UTC(), op.ID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return "", nil
	}
	have, found, err := readYTBinding(ctx, tx, taskID)
	if err != nil {
		return "", err
	}
	if found && have.Tenant == op.Tenant && have.IssueID == ytID {
		return "", nil
	}
	detail := fmt.Sprintf("issue %s on %s is bound to another task", ytID, op.Tenant)
	if found {
		detail = fmt.Sprintf("task %d is bound to issue %s on %s (%s)", taskID, have.IssueID, have.Tenant, have.Origin)
	}
	log.Printf("ytwrite.binding_conflict task_id=%d op_id=%d issue_id=%q detail=%q", taskID, op.ID, ytID, detail)
	return detail, nil
}

// YTIssueBackfillProject counts one hk project's tasks in a backfill.
type YTIssueBackfillProject struct {
	Project string `json:"project"`
	// Prepared: a new issue_create op (or, in a dry run, one would be).
	Prepared int `json:"prepared"`
	// Rejected: a new op rejected at once (invalid_issue).
	Rejected int `json:"rejected"`
	// Existing: the task already has its op, in any state.
	Existing int `json:"existing"`
	// Unmapped: the project maps to no YouTrack project.
	Unmapped int `json:"unmapped"`
}

// YTIssueBackfillReport is what `youtrack issues-backfill` did.
type YTIssueBackfillReport struct {
	Tenant   string                    `json:"tenant"`
	Project  string                    `json:"project,omitempty"`
	DryRun   bool                      `json:"dry_run"`
	Projects []*YTIssueBackfillProject `json:"projects"`
}

// BackfillYTIssueOps prepares the issue_create op of every task with no
// binding (only project's tasks when it is not ""), bound to tenant. It calls
// no YouTrack: adoption of an existing issue happens when the op is sent. A
// task that already has its op is counted and left alone, so a second run
// prepares nothing. Each task is its own transaction; a dry run writes
// nothing.
func (s *Store) BackfillYTIssueOps(ctx context.Context, tenant, project string, dryRun bool) (YTIssueBackfillReport, error) {
	r := YTIssueBackfillReport{Tenant: tenant, Project: project, DryRun: dryRun, Projects: []*YTIssueBackfillProject{}}
	if tenant == "" {
		return r, errors.New("backfill needs the YouTrack tenant")
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id FROM tasks t
		WHERE ($1 = '' OR t.project = $1)
		AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b WHERE b.task_id = t.id)
		ORDER BY t.id`, project)
	if err != nil {
		return r, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return r, err
	}
	byProject := map[string]*YTIssueBackfillProject{}
	for _, id := range ids {
		outcome, taskProject, err := s.backfillYTIssue(ctx, tenant, id, dryRun)
		if err != nil {
			return r, fmt.Errorf("task %d: %w", id, err)
		}
		p := byProject[taskProject]
		if p == nil {
			p = &YTIssueBackfillProject{Project: taskProject}
			byProject[taskProject] = p
			r.Projects = append(r.Projects, p)
		}
		switch outcome {
		case "prepared":
			p.Prepared++
		case "rejected":
			p.Rejected++
		case "existing":
			p.Existing++
		case "unmapped":
			p.Unmapped++
		}
	}
	return r, nil
}

// backfillYTIssue is one task of a backfill, in one transaction: "prepared",
// "rejected", "existing" or "unmapped", and the task's project.
func (s *Store) backfillYTIssue(ctx context.Context, tenant string, taskID int64, dryRun bool) (string, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var t Task
	if err := tx.QueryRow(ctx, `SELECT id, title, created_by, project FROM tasks WHERE id=$1`, taskID).Scan(&t.ID, &t.Title, &t.CreatedBy, &t.Project); err != nil {
		return "", "", err
	}
	name := deref(t.Project)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3)`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(taskID)).Scan(&exists); err != nil {
		return "", "", err
	}
	if exists {
		return "existing", name, nil
	}
	p, found, err := readYTIssueProject(ctx, tx, name)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "unmapped", name, nil
	}
	if dryRun {
		return "prepared", name, nil
	}
	op, created, err := prepareYTIssueTx(ctx, tx, time.Now().UTC(), t, p, tenant)
	if err != nil {
		return "", "", err
	}
	outcome := "prepared"
	switch {
	case !created:
		outcome = "existing"
	case op.State == ytwrite.StateRejected:
		outcome = "rejected"
	}
	return outcome, name, tx.Commit(ctx)
}

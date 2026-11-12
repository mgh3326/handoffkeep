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
	mode     YTIssuesMode
	tenant   string
	bucketID string
}

// SetYTIssuesMode sets the gate for this process and the YouTrack tenant a new
// task's op is bound to. serve calls it once at start, before any request. On
// with an empty tenant is off.
func (s *Store) SetYTIssuesMode(m YTIssuesMode, tenant string) {
	previous := s.ytIssues.Load()
	var bucketID string
	if previous != nil {
		bucketID = previous.bucketID
	}
	s.ytIssues.Store(&ytIssuesGate{mode: m, tenant: tenant, bucketID: bucketID})
}

// SetYTIssuesBucketProject sets the import's HK bucket project id. YouTrack
// owns this id; serve resolves it once before enabling issue writes.
func (s *Store) SetYTIssuesBucketProject(projectID string) {
	previous := s.ytIssues.Load()
	gate := ytIssuesGate{bucketID: projectID}
	if previous != nil {
		gate.mode, gate.tenant = previous.mode, previous.tenant
	}
	s.ytIssues.Store(&gate)
}

// ytIssuesNow is the gate: on and its tenant, or off.
func (s *Store) ytIssuesNow() (YTIssuesMode, string) {
	g := s.ytIssues.Load()
	if g == nil || g.mode != YTIssuesOn || g.tenant == "" {
		return YTIssuesOff, ""
	}
	return YTIssuesOn, g.tenant
}

func init() {
	registerYTKindGate(ytwrite.KindIssueCreate, ytIssueGateSQL)
	registerYTApplyHook(ytwrite.KindIssueCreate, ytApplyHook{conflict: ytIssueConflictTx, bind: bindYTIssueApplyTx})
}

// bindYTIssueApplyTx is the issue_create apply hook's bind: the task's
// binding, then its first field snapshot, which the new binding (no task
// change) would not otherwise reach: prepared here, behind the op on the
// task's lane (hk 4003).
func bindYTIssueApplyTx(s *Store, ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, adopted bool) error {
	if err := bindYTIssueTx(ctx, tx, op, ytID, now, adopted); err != nil {
		return err
	}
	taskID, _ := strconv.ParseInt(op.EntityID, 10, 64)
	return s.ytFieldsOnBindTx(ctx, tx, taskID, now)
}

// ytIssueGateSQL is the issue_create gate (ytAdmitSQL): mode on, and o's task
// has no binding or a binding this op published. A binding from the import
// (or another op) holds the op: the task already has its issue. Mode off
// admits nothing. Issue mode assumes the legacy `youtrack sync` refuses to
// write while it is on (hk 3907) and the import has ended (D3); nothing here
// coordinates with them (docs/youtrack-write-protocol.md).
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

func (s *Store) ytIssueProject(ctx context.Context, q ytRowQuerier, project string) (ytIssueProject, bool, error) {
	if project != "" {
		return readYTIssueProject(ctx, q, project)
	}
	g := s.ytIssues.Load()
	if g == nil || g.bucketID == "" {
		return ytIssueProject{}, false, nil
	}
	return ytIssueProject{project: "", shortName: "HK", projectID: g.bucketID}, true, nil
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

// enqueueYTIssueTx is every task creator's step in mode on (CreateTask,
// intake, ticket sets, dots, dispositions), after the task insert and before
// its commit: the task's issue_create op, or nothing (one log line)
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
	p, found, err := s.ytIssueProject(ctx, tx, deref(t.Project))
	if err != nil {
		return err
	}
	if !found {
		log.Printf("ytwrite.issue_skipped task_id=%d project=%q reason=project_unmapped", t.ID, deref(t.Project))
		return nil
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(t.ID, 10)); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3)`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(t.ID)).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	op, _, err := prepareYTIssueTx(ctx, tx, t.CreatedAt.UTC(), t, p, tenant)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.issue_enqueued op_id=%d task_id=%d state=%s reason=%q", op.ID, t.ID, op.State, op.Reason)
	return nil
}

// ytIssueConflictTx reads, in Apply's transaction (which holds the task's
// lane lock), whether publishing issue ytID for op's task would contradict a
// mapping hk already keeps: the task bound to another issue, the issue bound
// to another task, the task's map row naming another issue, a map row
// giving the issue to another task, or the task's YouTrack origin naming
// another issue. It returns the conflict ("" when none)
// and logs it as ytwrite.binding_conflict. A binding or map row naming this
// same issue is no conflict. Nothing is written.
func ytIssueConflictTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string) (string, error) {
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || taskID < 1 || op.EntityKind != "task" {
		return "", fmt.Errorf("issue_create op %d is not on a task lane (%s:%s)", op.ID, op.EntityKind, op.EntityID)
	}
	var detail string
	err = tx.QueryRow(ctx, `SELECT coalesce(
		(SELECT 'task is bound to issue '||issue_id||' on '||tenant||' ('||origin||')' FROM yt_task_bindings
			WHERE task_id=$1 AND (tenant<>$2 OR issue_id<>$3)),
		(SELECT 'issue is bound to task '||task_id FROM yt_task_bindings
			WHERE tenant=$2 AND issue_id=$3 AND task_id<>$1 LIMIT 1),
		(SELECT 'task is mapped to issue '||yt_id FROM yt_entity_map
			WHERE entity_kind='task' AND hk_key=$4 AND yt_id<>$3),
		(SELECT 'issue is mapped to task '||hk_key FROM yt_entity_map
			WHERE entity_kind='task' AND yt_id=$3 AND hk_key<>$4),
		(SELECT 'task''s YouTrack origin names issue '||coalesce(external_id,'')||' on '||coalesce(tenant,'') FROM task_origins
			WHERE task_id=$1 AND provider='youtrack' AND (coalesce(external_id,'')<>$3 OR (coalesce(tenant,'')<>'' AND tenant<>$2))),
		'')`, taskID, op.Tenant, ytID, op.EntityID).Scan(&detail)
	if err != nil {
		return "", err
	}
	if detail != "" {
		log.Printf("ytwrite.binding_conflict task_id=%d op_id=%d issue_id=%q detail=%q", taskID, op.ID, ytID, detail)
	}
	return detail, nil
}

// bindYTIssueTx publishes the binding of an issue_create op applied with
// issue ytID, after ytIssueConflictTx found no conflict and the map row was
// written. A binding of the task to this same issue is kept as it is. The
// readable id is not known here (the protocol carries only the stable id),
// so it is left empty.
//
// comments_from: for an issue this op created, 0, so the comment catch-up
// copies every comment the task has. For an adopted issue (made before the
// op, by the importer or the legacy push), the task's highest comment id now,
// as a verified binding gets it: the issue already holds that history, and
// copying it again would duplicate it.
func bindYTIssueTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, adopted bool) error {
	taskID, _ := strconv.ParseInt(op.EntityID, 10, 64)
	tag, err := tx.Exec(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, origin, op_id, comments_from)
		VALUES ($1,$2,$3,'',$4,$5,$6,'op_applied',$7,
			CASE WHEN $8 THEN (SELECT coalesce(max(id),0) FROM task_comments WHERE task_id=$1) ELSE 0 END)
		ON CONFLICT DO NOTHING`,
		taskID, op.Tenant, ytID, op.YTTargetID, op.Principal, now.UTC(), op.ID, adopted)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	have, found, err := readYTBinding(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if found && have.Tenant == op.Tenant && have.IssueID == ytID {
		return nil
	}
	// Another task's binding took the issue after the conflict check (its
	// writer holds that task's lane, not this one): roll the apply back; the
	// op is retried and the next check records the conflict.
	return fmt.Errorf("%w: issue %s on %s was bound to another task during the apply of op %d", ytwrite.ErrConflictingResult, ytID, op.Tenant, op.ID)
}

// ytIssueOpPendingTx reports whether task has an issue_create op that is not
// yet applied or rejected: the writers that give the task an issue another
// way (AddYTTaskBinding, AddYTCommentCanary) refuse while it does, so the op
// never races them to a second issue. The caller holds the task's lane lock.
func ytIssueOpPendingTx(ctx context.Context, q ytRowQuerier, taskID int64) (bool, error) {
	var pending bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2
		AND state NOT IN ('applied','rejected'))`, ytwrite.KindIssueCreate, strconv.FormatInt(taskID, 10)).Scan(&pending)
	return pending, err
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
	// Owned: the task already has an issue hk knows of (a binding, a canary
	// enrollment, a task map row or a YouTrack origin recorded earlier);
	// creating one would make a second.
	Owned int `json:"owned"`
}

// YTIssueBackfillReport is what `youtrack issues-backfill` did.
type YTIssueBackfillReport struct {
	Tenant   string                    `json:"tenant"`
	Project  string                    `json:"project,omitempty"`
	DryRun   bool                      `json:"dry_run"`
	Projects []*YTIssueBackfillProject `json:"projects"`
}

// BackfillYTIssueOps prepares the issue_create op of every task with no
// binding, no canary enrollment, no task map row and no YouTrack origin (only
// project's tasks when it is not ""), bound to tenant. It reads the origin
// row only; it does not coordinate with the legacy sync. It calls
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
		case "owned":
			p.Owned++
		}
	}
	return r, nil
}

// backfillYTIssue is one task of a backfill, in one transaction: "prepared",
// "rejected", "existing", "owned" or "unmapped", and the task's project. It
// takes the task's lane lock first, the lock canary enrollment and the
// comment path take before they give the task an issue, so an "owned" check
// and the op it allows cannot interleave with them.
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
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(taskID, 10)); err != nil {
		return "", "", err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3)`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(taskID)).Scan(&exists); err != nil {
		return "", "", err
	}
	if exists {
		return "existing", name, nil
	}
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1)
		OR EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$2)
		OR EXISTS(SELECT 1 FROM yt_task_bindings WHERE task_id=$1)
		OR EXISTS(SELECT 1 FROM task_origins WHERE task_id=$1 AND provider='youtrack')`, taskID, strconv.FormatInt(taskID, 10)).Scan(&owned); err != nil {
		return "", "", err
	}
	if owned {
		return "owned", name, nil
	}
	p, found, err := s.ytIssueProject(ctx, tx, name)
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

package store

import (
	"context"
	"encoding/json"
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
// admits nothing. The legacy `youtrack sync` and the import are gone (hk
// 4426), so the binding row is the whole coordination
// (docs/youtrack-write-protocol.md).
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
	// The boundary an adopted issue's binding starts its comment catch-up
	// from (hk 4301, bindYTIssueTx): the comments the task has now. The
	// boundary is exact: comment inserts hold taskCommentInsertLock until
	// they commit, so ids commit in order and no comment at or below the
	// highest committed id can commit after this read.
	if _, err := tx.Exec(ctx, `INSERT INTO yt_issue_comment_bounds(op_id, task_id, comments_from, recorded_at)
		VALUES ($1,$2,(SELECT coalesce(max(id),0) FROM task_comments WHERE task_id=$2),$3) ON CONFLICT (op_id) DO NOTHING`,
		op.ID, t.ID, now.UTC()); err != nil {
		return op, true, err
	}
	if cerr := ytwrite.CheckIssue(req); cerr != nil {
		op, err = rejectYTOpTx(ctx, tx, now, op.ID, ytwrite.ReasonInvalidIssue, cerr.Error())
	}
	return op, true, err
}

// enqueueYTIssueTx is every task creator's step in mode on (CreateTask,
// intake, ticket sets, dots, dispositions), after the task insert and before
// its commit: the task's issue_create op, or, when its project maps to no
// YouTrack project, an issue intent on the gate's tenant (hk 3942). Any
// database error fails the whole create.
func (s *Store) enqueueYTIssueTx(ctx context.Context, tx pgx.Tx, t Task) error {
	_, err := s.ensureYTIssueTx(ctx, tx, t, t.CreatedAt.UTC(), true)
	return err
}

// ensureYTIssueTx gives task t its issue_create op in mode on, under the
// task's lane lock, which it takes first: on the tenant of the task's
// yt_issue_intents row when it has one, else on the gate's tenant. A task
// that already has its op, or an issue some other way
// (ytTaskHasIssueOwnershipTx), gets nothing and loses its intent. A task
// whose project maps to no YouTrack project gets no op (one
// ytwrite.issue_skipped line); atCreate records the intent then, so the
// intent pass (PrepareMappedYTIssues) prepares the op once the project is
// mapped. An intent is made only at create time: a task moved to an unmapped
// project keeps the op or the intent it has (hk 4318). created reports a new
// op. A prepared op is never changed here: its claim targets the task's
// project then (claimYTIssueTargetTx).
//
// Locks, after the caller's (lockorder.go): the task's lane lock, then plain
// reads of its yt_issue_intents row, the ownership rows and
// youtrack_projects, then its new yt_write_ops row (prepareYTOpTx) and its
// yt_issue_intents row.
func (s *Store) ensureYTIssueTx(ctx context.Context, tx pgx.Tx, t Task, now time.Time, atCreate bool) (created bool, err error) {
	mode, tenant := s.ytIssuesNow()
	if mode != YTIssuesOn {
		return false, nil
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(t.ID, 10)); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3)`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(t.ID)).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, dropYTIssueIntentTx(ctx, tx, t.ID)
	}
	var intent string
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT tenant FROM yt_issue_intents WHERE task_id=$1),'')`, t.ID).Scan(&intent); err != nil {
		return false, err
	}
	if intent != "" {
		tenant = intent
	}
	owned, err := ytTaskHasIssueOwnershipTx(ctx, tx, t.ID)
	if err != nil {
		return false, err
	}
	if owned {
		log.Printf("ytwrite.issue_skipped task_id=%d project=%q reason=issue_owned", t.ID, deref(t.Project))
		return false, dropYTIssueIntentTx(ctx, tx, t.ID)
	}
	p, found, err := s.ytIssueProject(ctx, tx, deref(t.Project))
	if err != nil {
		return false, err
	}
	if !found {
		log.Printf("ytwrite.issue_skipped task_id=%d project=%q reason=project_unmapped", t.ID, deref(t.Project))
		if !atCreate {
			return false, nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO yt_issue_intents(task_id, tenant, recorded_at) VALUES ($1,$2,$3)
			ON CONFLICT (task_id) DO NOTHING`, t.ID, tenant, now.UTC())
		return false, err
	}
	op, created, err := prepareYTIssueTx(ctx, tx, now, t, p, tenant)
	if err != nil {
		return false, err
	}
	log.Printf("ytwrite.issue_enqueued op_id=%d task_id=%d state=%s reason=%q", op.ID, t.ID, op.State, op.Reason)
	return created, dropYTIssueIntentTx(ctx, tx, t.ID)
}

func dropYTIssueIntentTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM yt_issue_intents WHERE task_id=$1`, taskID)
	return err
}

// claimYTIssueTargetTx is the target and payload issue_create op op is
// dispatched with, in Claim's transaction under the task's lane lock
// (hk 3909, 4318): the YouTrack project the task's project maps to now, so a
// task moved before its issue was created gets it in its current project. The
// op keeps its id, its key (ytwrite.IssueKey) and its place on the lane, and
// only its yt_target_id, payload and payload_sha change, in the update that
// dispatches it (migration 20261113-yt-issue-claim-target); a release that
// does not retarget reads the same op and creates the issue where it was
// prepared. The op is dispatched as it is (payload nil) when its project is
// still the task's, when the task's project maps to no YouTrack project (or
// it has none and this process does not know the HK bucket), or when the task
// is gone. The task row is read without a lock (no lane holder locks a task
// row): a move that commits after this read finds the op dispatched, and the
// issue follows the task as any bound task's does (issue_move, yt_move.go).
func (s *Store) claimYTIssueTargetTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) (target, sha string, payload []byte, err error) {
	target, sha = op.YTTargetID, op.PayloadSHA
	if op.Kind != ytwrite.KindIssueCreate || op.Principal != YTIssuePrincipal || op.EntityKind != "task" {
		return target, sha, nil, nil
	}
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil {
		return target, sha, nil, nil
	}
	var project *string
	err = tx.QueryRow(ctx, `SELECT project FROM tasks WHERE id=$1`, taskID).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, sha, nil, nil
	}
	if err != nil {
		return "", "", nil, err
	}
	p, found, err := s.ytIssueProject(ctx, tx, deref(project))
	if err != nil || !found {
		return target, sha, nil, err
	}
	var was ytwrite.IssueCreatePayload
	if err := json.Unmarshal(op.Payload, &was); err != nil {
		return target, sha, nil, nil
	}
	if op.YTTargetID == p.projectID && was.Project == p.project {
		return target, sha, nil, nil
	}
	n, err := ytwrite.IssueOp(ytwrite.IssueRequest{Principal: op.Principal, Actor: op.Actor, TaskID: taskID, Title: was.Title,
		Project: p.project, YTProjectID: p.projectID, YTProjectShort: p.shortName, Tenant: op.Tenant})
	if err != nil {
		return "", "", nil, err
	}
	log.Printf("ytwrite.issue_retargeted op_id=%d task_id=%d from_project=%q to_project=%q yt_project_id=%q", op.ID, taskID, was.Project, p.project, p.projectID)
	return n.YTTargetID, n.PayloadSHA, n.Payload, nil
}

// PrepareMappedYTIssues prepares, for up to limit yt_issue_intents rows of
// the gate's tenant whose task's project now maps to a YouTrack project (or
// has no project while the HK bucket is known), the task's issue_create op
// through ensureYTIssueTx, each task in its own transaction, and returns how
// many ops it prepared (hk 3942). The write worker of that tenant runs it
// before each Drive pass in issue mode on, so the op goes out in that pass;
// an intent of another tenant is never consumed here.
func (s *Store) PrepareMappedYTIssues(ctx context.Context, limit int) (int, error) {
	mode, tenant := s.ytIssuesNow()
	if mode != YTIssuesOn {
		return 0, nil
	}
	bucket := ""
	if g := s.ytIssues.Load(); g != nil {
		bucket = g.bucketID
	}
	rows, err := s.pool.Query(ctx, `SELECT i.task_id FROM yt_issue_intents i JOIN tasks t ON t.id = i.task_id
		WHERE i.tenant = $1 AND (
			(coalesce(t.project,'') = '' AND $2 <> '')
			OR EXISTS (SELECT 1 FROM youtrack_projects yp WHERE yp.hk_project = t.project
				AND coalesce(yp.yt_project_id,'') <> '' AND coalesce(yp.short_name,'') <> ''))
		ORDER BY i.task_id LIMIT $3`, tenant, bucket, ytLimit(limit))
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		prepared, err := s.prepareYTIssueIntent(ctx, tenant, id)
		if err != nil {
			return n, fmt.Errorf("task %d: %w", id, err)
		}
		if prepared {
			n++
		}
	}
	return n, nil
}

// prepareYTIssueIntent is one task of PrepareMappedYTIssues: the task's lane
// lock first, as backfillYTIssue takes it, then the task row read without a
// lock (after the lane lock, so it sees the project a SetTaskProject
// committed under that lane, or that change waits and finds the op), then
// ensureYTIssueTx. An intent that moved to another tenant meanwhile is left
// alone. prepared reports a new op.
func (s *Store) prepareYTIssueIntent(ctx context.Context, tenant string, taskID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(taskID, 10)); err != nil {
		return false, err
	}
	var have string
	if err := tx.QueryRow(ctx, `SELECT tenant FROM yt_issue_intents WHERE task_id=$1`, taskID).Scan(&have); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	} else if have != tenant {
		return false, nil
	}
	var t Task
	if err := tx.QueryRow(ctx, `SELECT id, title, created_by, project FROM tasks WHERE id=$1`, taskID).Scan(&t.ID, &t.Title, &t.CreatedBy, &t.Project); err != nil {
		return false, err
	}
	created, err := s.ensureYTIssueTx(ctx, tx, t, time.Now().UTC(), false)
	if err != nil {
		return false, err
	}
	return created, tx.Commit(ctx)
}

// ytTaskHasIssueOwnershipTx mirrors the task ownership evidence used by
// issue backfill. Any of these records means an issue already belongs to the
// task, even when the canonical binding has not been published yet. So does an
// operator unbind (hk 4490): a task whose latest yt_binding_history row is
// 'unbound' is the operator's until they bind it again, and no automatic
// issue_create op is prepared for it, on create, on a project change or by the
// backfill. The callers hold the task's lane lock, which the unbind holds when
// it appends that row.
func ytTaskHasIssueOwnershipTx(ctx context.Context, q ytRowQuerier, taskID int64) (bool, error) {
	var owned bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1)
		OR EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$2)
		OR EXISTS(SELECT 1 FROM yt_task_bindings WHERE task_id=$1)
		OR EXISTS(SELECT 1 FROM task_origins WHERE task_id=$1 AND provider='youtrack')
		OR coalesce((SELECT action FROM yt_binding_history WHERE task_id=$1 ORDER BY id DESC LIMIT 1), '') = '`+ytBindingActionUnbound+`'`,
		taskID, strconv.FormatInt(taskID, 10)).Scan(&owned)
	return owned, err
}

// ytIssueConflictTx reads, in Apply's transaction (which holds the task's
// lane lock), whether publishing issue ytID for op's task would contradict a
// mapping hk already keeps: the task bound to another issue, the issue bound
// to another task, the task's map row naming another issue, a map row
// giving the issue to another task, or the task's YouTrack origin naming
// another issue. It returns the conflict ("" when none)
// and logs it as ytwrite.binding_conflict. A binding or map row naming this
// same issue is no conflict, and neither is a map row on another tenant: a
// YouTrack id is the tenant's (hk 3912), and a row with no tenant (one a
// release that predates the column wrote) counts as this one's. Nothing is
// written.
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
			WHERE entity_kind='task' AND (tenant=$2 OR tenant='') AND yt_id=$3 AND hk_key<>$4),
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
// op, by the importer or the legacy push), the task's highest comment id when
// the op was prepared (yt_issue_comment_bounds, hk 4301): the issue already
// holds the history before it, and copying that again would duplicate it,
// but nothing copied a comment written while the op was pending (under hk
// comments authority it got no op), so the catch-up copies those. An op
// prepared by a release before that table has no row and gets the task's
// highest comment id now, as a verified binding gets it.
func bindYTIssueTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, adopted bool) error {
	taskID, _ := strconv.ParseInt(op.EntityID, 10, 64)
	tag, err := tx.Exec(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, origin, op_id, comments_from)
		VALUES ($1,$2,$3,'',$4,$5,$6,'op_applied',$7,
			CASE WHEN $8 THEN coalesce((SELECT comments_from FROM yt_issue_comment_bounds WHERE op_id=$7),
				(SELECT coalesce(max(id),0) FROM task_comments WHERE task_id=$1)) ELSE 0 END)
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
	// OtherTenant: the task waits on an issue intent of another tenant
	// (hk 3942); that tenant's worker prepares its op.
	OtherTenant int `json:"other_tenant"`
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
		case "other_tenant":
			p.OtherTenant++
		}
	}
	return r, nil
}

// backfillYTIssue is one task of a backfill, in one transaction: "prepared",
// "rejected", "existing", "owned", "other_tenant" or "unmapped", and the
// task's project. A prepared op drops the task's intent. It
// takes the task's lane lock first, the lock canary enrollment and the
// comment path take before they give the task an issue, so an "owned" check
// and the op it allows cannot interleave with them.
func (s *Store) backfillYTIssue(ctx context.Context, tenant string, taskID int64, dryRun bool) (string, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(taskID, 10)); err != nil {
		return "", "", err
	}
	// The task is read under the lane lock, after any SetTaskProject that
	// held it has committed, so the op and the intent it drops follow the
	// task's current project (PR #357 review).
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
	owned, err := ytTaskHasIssueOwnershipTx(ctx, tx, taskID)
	if err != nil {
		return "", "", err
	}
	if owned {
		return "owned", name, nil
	}
	// A task waiting on an issue intent belongs to the intent's tenant
	// (hk 3942): only that tenant's op may be prepared for it here.
	var intent string
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT tenant FROM yt_issue_intents WHERE task_id=$1),'')`, taskID).Scan(&intent); err != nil {
		return "", "", err
	}
	if intent != "" && intent != tenant {
		return "other_tenant", name, nil
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
	if err := dropYTIssueIntentTx(ctx, tx, taskID); err != nil {
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

package store

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Task fields mode (hk 4003, docs/youtrack-write-protocol.md "Kind
// issue_fields"): after the import ended, a task's state, priority, lane and
// the other fields the import set reach YouTrack only through this path. The
// change-feed consumer task_fields reads every task change from sync_log and,
// for a task bound to its issue (yt_task_bindings), prepares one issue_fields
// op with the task's full snapshot when the snapshot changed
// (prepareYTSnapshotTx). A task whose hk project moved to another mapped
// YouTrack project gets an issue_move op first (yt_move.go). The ops ride the
// task's lane (task:<id>) with its comments and its issue_create, so the
// writes to one issue stay in order.

// YTFieldsMode gates the task-fields projection (HANDOFFKEEP_YT_FIELDS_MODE).
type YTFieldsMode string

const (
	// YTFieldsOff prepares no issue_fields op and admits none (the default).
	// The consumer's cursor still advances: a change made while off reaches
	// YouTrack with the task's next change.
	YTFieldsOff YTFieldsMode = "off"
	// YTFieldsOn prepares an issue_fields op for each changed snapshot of a
	// bound task and admits issue_fields ops whose target is the task's
	// bound issue.
	YTFieldsOn YTFieldsMode = "on"
)

// YTFieldsPrincipal owns (and is the actor of) the issue_fields ops; their
// key is SnapshotKey(kind, task id, seq, sha).
const YTFieldsPrincipal = "hk:task-fields"

// ytFieldsConsumer is the change-feed consumer's name (its cursor's key).
const ytFieldsConsumer = "task_fields"

type ytFieldsGate struct {
	mode   YTFieldsMode
	tenant string
}

// SetYTFieldsMode sets the gate for this process and the YouTrack tenant
// whose bindings it writes to. serve calls it once at start. On with an
// empty tenant is off.
func (s *Store) SetYTFieldsMode(m YTFieldsMode, tenant string) {
	s.ytFields.Store(&ytFieldsGate{mode: m, tenant: tenant})
}

// ytFieldsNow is the gate: on and its tenant, or off.
func (s *Store) ytFieldsNow() (YTFieldsMode, string) {
	g := s.ytFields.Load()
	if g == nil || g.mode != YTFieldsOn || g.tenant == "" {
		return YTFieldsOff, ""
	}
	return YTFieldsOn, g.tenant
}

func init() {
	registerYTKindGate(ytwrite.KindIssueFields, ytFieldsGateSQL)
	registerYTFeedStoreConsumer(ytFieldsConsumer, []string{"task"}, ytFieldsHandler)
}

// ytFieldsGateSQL is the issue_fields gate (ytAdmitSQL), and the issue_move
// gate too (yt_move.go): mode on, and o's target is the issue o's task is
// bound to on o's tenant now. An op whose task was rebound stays prepared; a
// project move keeps the issue, so its own gate is this one. Mode off admits
// nothing.
func ytFieldsGateSQL(s *Store) string {
	if mode, _ := s.ytFieldsNow(); mode != YTFieldsOn {
		return `FALSE`
	}
	return `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// ytFieldsRow is one task as the snapshot reads it, with its binding and
// the YouTrack project its hk project maps to.
type ytFieldsRow struct {
	fields ytwrite.TaskFields
	// binding: tenant, issue and YouTrack project ("" when unbound);
	// mappedProjectID: youtrack_projects.yt_project_id of the task's
	// project ("" when unmapped); projectAgrees: the task's project still
	// maps to the binding's YouTrack project, or the task has no project and
	// the binding's project is one no row maps (the import's HK bucket for
	// such tasks), the rule ytBindingDisagreement applies.
	tenant, issueID, bindingProjectID, mappedProjectID string
	projectAgrees                                      bool
}

// ytFieldsSelectSQL reads the snapshot inputs of the tasks in $1: the row,
// its activity (for 멈춤), its current Korean title, its milestone's name,
// its binding, its project's YouTrack project and whether that agrees with
// the binding (projectAgrees).
const ytFieldsSelectSQL = `SELECT tasks.id, tasks.title, tasks.state, tasks.priority, tasks.lane, tasks.kind, tasks.refs,
	tasks.body_doc, coalesce(tasks.project,''), coalesce(tasks.area,''), coalesce(tasks.expected,''), coalesce(tasks.parent_id,0),
	tasks.due_at, tasks.awaiting_decision, ` + taskActivitySQL + `,
	coalesce((SELECT tr.text FROM translations tr WHERE tr.entity='task' AND tr.entity_id=tasks.id::text AND tr.field='title'
		AND tr.lang='ko' AND tr.source_sha256=encode(sha256(convert_to(tasks.title,'UTF8')),'hex') LIMIT 1),''),
	coalesce((SELECT m.name FROM project_milestones m WHERE m.id=tasks.milestone_id),''),
	coalesce(b.tenant,''), coalesce(b.issue_id,''), coalesce(b.project_id,''),
	coalesce((SELECT p.yt_project_id FROM youtrack_projects p WHERE p.hk_project=tasks.project),''),
	(EXISTS(SELECT 1 FROM youtrack_projects p WHERE p.hk_project=tasks.project AND p.yt_project_id=b.project_id)
		OR (coalesce(tasks.project,'')='' AND NOT EXISTS(SELECT 1 FROM youtrack_projects p WHERE p.yt_project_id=b.project_id)))
	FROM tasks LEFT JOIN yt_task_bindings b ON b.task_id=tasks.id
	WHERE tasks.id = ANY($1::bigint[])`

// readYTFieldsRows reads the snapshot inputs of ids at now, by task id; a
// deleted task is absent.
func readYTFieldsRows(ctx context.Context, q poolQuerier, ids []int64, now time.Time, stalledAfter time.Duration) (map[int64]ytFieldsRow, error) {
	rows, err := q.Query(ctx, ytFieldsSelectSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]ytFieldsRow, len(ids))
	for rows.Next() {
		var r ytFieldsRow
		f := &r.fields
		var refsJSON []byte
		var expected string
		var due *time.Time
		var awaiting bool
		var activity time.Time
		if err := rows.Scan(&f.TaskID, &f.Title, &f.State, &f.Priority, &f.Lane, &f.Kind, &refsJSON, &f.BodyDoc, &f.Project, &f.Area,
			&expected, &f.ParentID, &due, &awaiting, &activity, &f.TitleKO, &f.Milestone,
			&r.tenant, &r.issueID, &r.bindingProjectID, &r.mappedProjectID, &r.projectAgrees); err != nil {
			return nil, err
		}
		var refs TaskRefs
		if len(refsJSON) > 0 {
			if err := json.Unmarshal(refsJSON, &refs); err != nil {
				return nil, err
			}
		}
		f.PR, f.JobID = refs.PR, refs.JobID
		if refs.Linear != nil {
			f.LinearGrade, f.LinearTier = refs.Linear.Grade, refs.Linear.Tier
		}
		if due != nil {
			d := due.UTC()
			f.DueAt = &d
		}
		if normalized, ok := normalizeExpected(expected); ok {
			f.Expected, _ = time.ParseDuration(normalized)
		}
		f.Stalled = isStalled(f.State, awaiting, activity, now, stalledAfter)
		out[f.TaskID] = r
	}
	return out, rows.Err()
}

// ytFieldsOutcome is what prepareYTFieldsTx did with one task.
type ytFieldsOutcome string

const (
	ytFieldsPrepared    ytFieldsOutcome = "prepared"
	ytFieldsUnchanged   ytFieldsOutcome = "unchanged"
	ytFieldsDeleted     ytFieldsOutcome = "deleted"
	ytFieldsUnbound     ytFieldsOutcome = "unbound"
	ytFieldsOtherTenant ytFieldsOutcome = "other_tenant"
	ytFieldsMoved       ytFieldsOutcome = "moved"
	ytFieldsInvalid     ytFieldsOutcome = "invalid"
)

// ytFieldsSkipOutcome is why the snapshot rule writes nothing for a task that
// is not one of this worker's bound tasks: a deleted task (found false) is
// skipped; a task with no binding is skipped (its issue_create publishes the
// binding and prepares the first snapshot then, ytFieldsOnBindTx); a binding on
// another tenant is skipped. It is "" for every other task.
func ytFieldsSkipOutcome(tenant string, t ytFieldsRow, found bool) ytFieldsOutcome {
	switch {
	case !found:
		return ytFieldsDeleted
	case t.issueID == "":
		return ytFieldsUnbound
	case t.tenant != tenant:
		return ytFieldsOtherTenant
	}
	return ""
}

// prepareYTFieldsTx decides one task's snapshot in tx at now, for the worker
// of tenant. A task that is not bound on this tenant is skipped
// (ytFieldsSkipOutcome). A task whose hk project no longer maps to its
// binding's YouTrack project gets an issue_move op first (ytMoveForTx), and its
// snapshot names the project the issue belongs in; with no mapped target
// nothing is prepared for it. A snapshot the protocol cannot store is skipped
// and logged. Every other task prepares its snapshot through
// prepareYTSnapshotTx, which makes no op when the snapshot is the one last
// prepared.
func prepareYTFieldsTx(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, id int64, t ytFieldsRow, found bool) (ytFieldsOutcome, error) {
	if skip := ytFieldsSkipOutcome(tenant, t, found); skip != "" {
		return skip, nil
	}
	// The project the issue belongs in, and the move to it when the issue is
	// not there yet (yt_move.go). The snapshot names that project: it is
	// written after the move, on the same lane.
	project, moving, err := ytMoveForTx(ctx, tx, now, id, t)
	if errors.Is(err, ytwrite.ErrInvalid) {
		log.Printf("ytwrite.move_skipped task_id=%d reason=invalid err=%q", id, err.Error())
		return ytFieldsInvalid, nil
	}
	if err != nil {
		return "", err
	}
	if project == "" {
		return ytFieldsMoveSkipped, nil
	}
	n, outcome, err := ytFieldsOpFor(id, t, project)
	if outcome != "" || err != nil {
		return outcome, err
	}
	op, made, err := prepareYTSnapshotTx(ctx, tx, now, n, n.PayloadSHA)
	if err != nil {
		return "", err
	}
	if made {
		log.Printf("ytwrite.fields_enqueued op_id=%d task_id=%d issue_id=%q", op.ID, id, t.issueID)
	}
	switch {
	case moving:
		return ytFieldsMoved, nil
	case !made:
		return ytFieldsUnchanged, nil
	}
	return ytFieldsPrepared, nil
}

// ytFieldsOpFor is task id's snapshot op for the issue in project (its project
// after any move). A snapshot the protocol cannot store is skipped and logged
// (ytFieldsInvalid); every other task returns its op and "".
func ytFieldsOpFor(id int64, t ytFieldsRow, project string) (ytwrite.NewOp, ytFieldsOutcome, error) {
	n, err := ytwrite.FieldsOp(ytwrite.FieldsRequest{Principal: YTFieldsPrincipal, Actor: YTFieldsPrincipal, Tenant: t.tenant,
		IssueID: t.issueID, ProjectID: project, Task: t.fields})
	if errors.Is(err, ytwrite.ErrInvalid) {
		// Bad data must not stop the feed: the cursor would never pass
		// this row.
		log.Printf("ytwrite.fields_skipped task_id=%d reason=invalid err=%q", id, err.Error())
		return ytwrite.NewOp{}, ytFieldsInvalid, nil
	}
	if err != nil {
		return ytwrite.NewOp{}, "", err
	}
	return n, "", nil
}

// ytFieldsOnBindTx prepares the first snapshot of a task whose binding an
// applied issue_create op just published in tx (hk 4003 review): the binding
// is no task change, so the feed would not see it, and the issue_create wrote
// only the title, hk ID and project. The snapshot reads the task as it is
// now, so changes the feed consumed while the task was unbound are in it. The
// caller holds the task's lane lock (Apply), which prepareYTSnapshotTx takes
// again; the op queues after the issue_create on that lane. Mode off, or a
// binding on another tenant than the gate's, prepares nothing.
func (s *Store) ytFieldsOnBindTx(ctx context.Context, tx pgx.Tx, taskID int64, now time.Time) error {
	mode, tenant := s.ytFieldsNow()
	if mode != YTFieldsOn {
		return nil
	}
	tasks, err := readYTFieldsRows(ctx, tx, []int64{taskID}, now, s.StartedStalledHours())
	if err != nil {
		return err
	}
	t, found := tasks[taskID]
	outcome, err := prepareYTFieldsTx(ctx, tx, now, tenant, taskID, t, found)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.fields_on_bind task_id=%d outcome=%s", taskID, outcome)
	return nil
}

// ytFieldsHandler is the task_fields consumer's handler for one pass at now.
// Mode off does nothing (the cursor advances). Otherwise each changed task,
// in sync_id order, goes through prepareYTFieldsTx.
func ytFieldsHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		mode, tenant := s.ytFieldsNow()
		if mode != YTFieldsOn {
			return nil
		}
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			if id, err := strconv.ParseInt(r.ModelID, 10, 64); err == nil && id > 0 && r.Model == "task" {
				ids = append(ids, id)
			}
		}
		tasks, err := readYTFieldsRows(ctx, tx, ids, now, s.StartedStalledHours())
		if err != nil {
			return err
		}
		counts := map[ytFieldsOutcome]int{}
		for _, id := range ids {
			t, found := tasks[id]
			outcome, err := prepareYTFieldsTx(ctx, tx, now, tenant, id, t, found)
			if err != nil {
				return err
			}
			counts[outcome]++
		}
		log.Printf("ytwrite.fields_feed tasks=%d prepared=%d unchanged=%d deleted=%d unbound=%d other_tenant=%d moved=%d move_skipped=%d invalid=%d",
			len(ids), counts[ytFieldsPrepared], counts[ytFieldsUnchanged], counts[ytFieldsDeleted], counts[ytFieldsUnbound],
			counts[ytFieldsOtherTenant], counts[ytFieldsMoved], counts[ytFieldsMoveSkipped], counts[ytFieldsInvalid])
		return nil
	}
}

package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A task's waits say whom the task waits on, for what, and since when (hk 5128,
// design hk 5112 §3, §8.2 and §9). Two kinds of wait feed one read:
//
//   - Derived waits are computed when they are read and never stored: an open
//     decision request waits on the operator, a blocks relation from an
//     unsettled task waits on that task, a queued spawn waits on its start
//     condition, and an open linked change waits on its PR. Nothing has to be
//     reconciled when a source changes.
//   - Hand waits are rows of task_waits, written by a session for a wait the
//     store cannot derive (an operator step, an external dependency, a failed
//     spawn). A hand wait is cleared, never deleted, so the record of how it
//     ended stays.
//
// The read (ListWaits) sorts both kinds into four sections. It is read-only: it
// answers no decision and changes no task.

// Wait kinds: the party a wait is on (task_waits.on_kind).
const (
	WaitOnOperator = "operator"
	WaitOnLane     = "lane"
	WaitOnTask     = "task"
	WaitOnPR       = "pr"
	WaitOnCI       = "ci"
	WaitOnExternal = "external"
)

// waitOnKinds is the wire order of the wait kinds, quoted in refusals.
var waitOnKinds = []string{WaitOnOperator, WaitOnLane, WaitOnTask, WaitOnPR, WaitOnCI, WaitOnExternal}

// WaitRefSpawn is the external ref a stopped spawn is recorded under: a session
// records a failed start with tasks wait --on external:spawn.
const WaitRefSpawn = "spawn"

// The four sections of the waits read (decision 402). Their keys are the wire
// values of the section query parameter.
const (
	WaitSectionNeedsOperator = "needs_operator"
	WaitSectionFailedStart   = "failed_start"
	WaitSectionWaitingOn     = "waiting_on"
	WaitSectionIdleStarted   = "idle_started"
)

// waitSectionKeys is the section order of the read.
var waitSectionKeys = []string{WaitSectionNeedsOperator, WaitSectionFailedStart, WaitSectionWaitingOn, WaitSectionIdleStarted}

// Bounds of a hand wait's text, of the actor name, and of the backlog the read
// carries: at most waitBacklogRows rows are listed from the first
// waitBacklogScan backlog tasks.
const (
	maxWaitRefBytes    = 256
	maxWaitReasonBytes = 1000
	maxWaitNoteBytes   = 1000
	maxWaitByBytes     = 128
	waitBacklogRows    = 50
	waitBacklogScan    = 1000
)

// ErrTaskWaitInvalid refuses a wait the store cannot hold. Its text is the wire
// code (TestErrorRuleCodesMatchSentinels); the reason after "invalid_context: "
// is store-authored and is returned to the caller.
var ErrTaskWaitInvalid = errors.New("invalid_context")

// ErrTaskWaitNotFound names a wait id that does not belong to the task.
var ErrTaskWaitNotFound = errors.New("task_wait_not_found")

func taskWaitInvalid(reason string) error { return fmt.Errorf("%w: %s", ErrTaskWaitInvalid, reason) }

// TaskWait is one hand wait as the server shows it.
type TaskWait struct {
	ID        int64      `json:"id"`
	TaskID    int64      `json:"task_id"`
	OnKind    string     `json:"on_kind"`
	OnRef     string     `json:"on_ref"`
	Reason    string     `json:"reason"`
	Since     time.Time  `json:"since"`
	Source    string     `json:"source"`
	ClearedAt *time.Time `json:"cleared_at,omitempty"`
	ClearedBy string     `json:"cleared_by,omitempty"`
	ClearNote string     `json:"clear_note,omitempty"`
}

// TaskWaitInput is the body of POST /v1/tasks/{id}/waits. on_ref is empty for an
// operator wait and names the lane, task id, PR, check or external dependency
// otherwise.
type TaskWaitInput struct {
	OnKind string `json:"on_kind"`
	OnRef  string `json:"on_ref"`
	Reason string `json:"reason"`
}

// otherTaskID parses the on_ref of a task wait: a positive task id other than
// self. ok is false when ref is not such an id.
func otherTaskID(ref string, self int64) (int64, bool) {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != ref || id == self {
		return 0, false
	}
	return id, true
}

// checkTaskWaitInput refuses a hand wait the table cannot hold. A task on_ref is
// checked against the store in the transaction.
func checkTaskWaitInput(taskID int64, in TaskWaitInput) error {
	known := false
	for _, k := range waitOnKinds {
		if k == in.OnKind {
			known = true
		}
	}
	switch {
	case !known:
		return taskWaitInvalid("on_kind is not a wait kind; want one of " + strings.Join(waitOnKinds, ", "))
	case in.OnKind == WaitOnOperator && in.OnRef != "":
		return taskWaitInvalid("an operator wait takes no on_ref")
	case in.OnKind != WaitOnOperator && (in.OnRef == "" || !validText(in.OnRef, maxWaitRefBytes)):
		return taskWaitInvalid(fmt.Sprintf("on_ref is required for %s, at most %d bytes", in.OnKind, maxWaitRefBytes))
	case in.OnKind == WaitOnTask:
		if _, ok := otherTaskID(in.OnRef, taskID); !ok {
			return taskWaitInvalid("on_ref for a task wait must name another task id")
		}
	case in.Reason == "" || !validText(in.Reason, maxWaitReasonBytes):
		return taskWaitInvalid(fmt.Sprintf("reason is required, at most %d bytes", maxWaitReasonBytes))
	}
	for _, text := range []string{in.OnRef, in.Reason} {
		if err := guard.Reject(text); err != nil {
			return err
		}
	}
	return nil
}

// AddTaskWait records a hand wait of task id, written by author. Repeating an
// open wait of the same kind and ref updates its reason and changes nothing
// else, so a session that says the same thing twice holds one wait.
func (s *Store) AddTaskWait(ctx context.Context, id int64, in TaskWaitInput, author string) (TaskWait, error) {
	if id < 1 {
		return TaskWait{}, ErrTaskNotFound
	}
	if author == "" || !validText(author, maxWaitByBytes) {
		return TaskWait{}, taskWaitInvalid(fmt.Sprintf("author is required, at most %d bytes", maxWaitByBytes))
	}
	if err := checkTaskWaitInput(id, in); err != nil {
		return TaskWait{}, err
	}
	var out TaskWait
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.addTaskWaitOnce(ctx, id, in, author, time.Now().UTC())
		return err
	})
	return out, err
}

// addTaskWaitOnce is one whole AddTaskWait transaction (retried on MySQL after a
// deadlock or lock-wait timeout).
func (s *Store) addTaskWaitOnce(ctx context.Context, id int64, in TaskWaitInput, author string, now time.Time) (TaskWait, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskWait{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	// The task row is read under a share lock, so the task cannot be deleted
	// while its wait is written (the same lock the spawn record takes).
	var locked int64
	err = tx.QueryRow(ctx, s.q(`SELECT id FROM tasks WHERE id=$1 FOR KEY SHARE`, `SELECT id FROM tasks WHERE id=? FOR SHARE`), id).Scan(&locked)
	if errors.Is(err, ErrNoRows) {
		return TaskWait{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskWait{}, err
	}
	if in.OnKind == WaitOnTask {
		other, _ := otherTaskID(in.OnRef, id)
		var exists bool
		if err := tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`), other).Scan(&exists); err != nil {
			return TaskWait{}, err
		}
		if !exists {
			return TaskWait{}, taskWaitInvalid("on_ref names no task")
		}
	}
	_, err = tx.Exec(ctx, sqlFor(tx,
		`INSERT INTO task_waits(task_id,on_kind,on_ref,reason,since,source) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(task_id,on_kind,on_ref) WHERE cleared_at IS NULL DO UPDATE SET reason=EXCLUDED.reason, source=EXCLUDED.source`,
		`INSERT INTO task_waits(task_id,on_kind,on_ref,reason,since,source) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE reason=VALUES(reason), source=VALUES(source)`),
		id, in.OnKind, in.OnRef, in.Reason, now, "session:"+author)
	if err != nil {
		return TaskWait{}, err
	}
	w, err := readOpenTaskWait(ctx, tx, id, in.OnKind, in.OnRef)
	if err != nil {
		return TaskWait{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TaskWait{}, err
	}
	return w, nil
}

// taskWaitColumns is the column list scanTaskWait reads, in its order.
const taskWaitColumns = `id,task_id,on_kind,on_ref,reason,since,source,cleared_at,cleared_by,clear_note`

// readOpenTaskWait reads the open hand wait of one kind and ref on task id.
func readOpenTaskWait(ctx context.Context, q rowQuerier, id int64, kind, ref string) (TaskWait, error) {
	return scanTaskWait(q.QueryRow(ctx, sqlFor(q,
		`SELECT `+taskWaitColumns+` FROM task_waits WHERE task_id=$1 AND on_kind=$2 AND on_ref=$3 AND cleared_at IS NULL`,
		`SELECT `+taskWaitColumns+` FROM task_waits WHERE task_id=? AND on_kind=? AND on_ref=? AND cleared_at IS NULL`),
		id, kind, ref))
}

// scanTaskWait reads one task_waits row in taskWaitColumns order.
func scanTaskWait(r row) (TaskWait, error) {
	var w TaskWait
	var clearedAt *time.Time
	if err := r.Scan(&w.ID, &w.TaskID, &w.OnKind, &w.OnRef, &w.Reason, &w.Since, &w.Source, &clearedAt, &w.ClearedBy, &w.ClearNote); err != nil {
		return TaskWait{}, err
	}
	w.Since = w.Since.UTC()
	if clearedAt != nil {
		t := clearedAt.UTC()
		w.ClearedAt = &t
	}
	return w, nil
}

// ClearTaskWait ends hand wait waitID of task id, recording by and note. It
// answers the wait and whether this call changed it: clearing a wait that has
// already ended changes nothing and answers it as it was.
func (s *Store) ClearTaskWait(ctx context.Context, id, waitID int64, by, note string) (TaskWait, bool, error) {
	if id < 1 || waitID < 1 {
		return TaskWait{}, false, ErrTaskWaitNotFound
	}
	if by == "" || !validText(by, maxWaitByBytes) {
		return TaskWait{}, false, taskWaitInvalid(fmt.Sprintf("by is required, at most %d bytes", maxWaitByBytes))
	}
	if !validText(note, maxWaitNoteBytes) {
		return TaskWait{}, false, taskWaitInvalid(fmt.Sprintf("note is at most %d bytes", maxWaitNoteBytes))
	}
	if err := guard.Reject(note); err != nil {
		return TaskWait{}, false, err
	}
	var out TaskWait
	var changed bool
	err := s.retryTx(ctx, func() error {
		var err error
		out, changed, err = s.clearTaskWaitOnce(ctx, id, waitID, by, note, time.Now().UTC())
		return err
	})
	return out, changed, err
}

// clearTaskWaitOnce is one whole ClearTaskWait transaction.
func (s *Store) clearTaskWaitOnce(ctx context.Context, id, waitID int64, by, note string, now time.Time) (TaskWait, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskWait{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	w, err := scanTaskWait(tx.QueryRow(ctx, sqlFor(tx,
		`SELECT `+taskWaitColumns+` FROM task_waits WHERE id=$1 AND task_id=$2 FOR UPDATE`,
		`SELECT `+taskWaitColumns+` FROM task_waits WHERE id=? AND task_id=? FOR UPDATE`),
		waitID, id))
	if errors.Is(err, ErrNoRows) {
		return TaskWait{}, false, ErrTaskWaitNotFound
	}
	if err != nil {
		return TaskWait{}, false, err
	}
	if w.ClearedAt != nil {
		return w, false, nil
	}
	if _, err := tx.Exec(ctx, sqlFor(tx,
		`UPDATE task_waits SET cleared_at=$1, cleared_by=$2, clear_note=$3 WHERE id=$4`,
		`UPDATE task_waits SET cleared_at=?, cleared_by=?, clear_note=? WHERE id=?`),
		now, by, note, waitID); err != nil {
		return TaskWait{}, false, err
	}
	w.ClearedAt, w.ClearedBy, w.ClearNote = &now, by, note
	if err := tx.Commit(ctx); err != nil {
		return TaskWait{}, false, err
	}
	return w, true, nil
}

// WaitRow is one waiting item in the read: a derived or a hand wait, the task
// it belongs to, and what it waits on. Key names the item, so a link opens one
// row.
type WaitRow struct {
	Key         string      `json:"key"`
	Section     string      `json:"section"`
	Source      string      `json:"source"`
	TaskID      int64       `json:"task_id"`
	Title       string      `json:"title"`
	Project     string      `json:"project,omitempty"`
	Lane        string      `json:"lane"`
	State       string      `json:"state"`
	OnKind      string      `json:"on_kind,omitempty"`
	OnRef       string      `json:"on_ref,omitempty"`
	Reason      string      `json:"reason"`
	Since       time.Time   `json:"since"`
	AgeSeconds  int64       `json:"age_seconds"`
	NextAction  string      `json:"next_action"`
	GroundsLink string      `json:"grounds_link,omitempty"`
	RequestID   string      `json:"request_id,omitempty"`
	Answer      *WaitAnswer `json:"answer,omitempty"`
}

// WaitAnswer is an answer recorded against an open decision request that is not
// yet confirmed (a pending_confirmation attempt, the latest for the request).
// The request stays open; the row shows the words rather than a plain question.
type WaitAnswer struct {
	Result string    `json:"result"`
	Words  string    `json:"words"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

// WaitSection is one of the four sections. Count is the number of waits in it;
// Rows carries them unless a section filter leaves the section out.
type WaitSection struct {
	Key       string    `json:"key"`
	Count     int       `json:"count"`
	Truncated bool      `json:"truncated,omitempty"`
	Rows      []WaitRow `json:"rows"`
}

// WaitBacklogRow is one ordinary backlog task: it has no wait in any section.
type WaitBacklogRow struct {
	TaskID    int64     `json:"task_id"`
	Title     string    `json:"title"`
	Lane      string    `json:"lane"`
	Project   string    `json:"project,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// WaitBacklog is the ordinary backlog, listed apart from the sections. Count
// covers the first waitBacklogScan backlog tasks; Truncated says there were more.
type WaitBacklog struct {
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated,omitempty"`
	Rows      []WaitBacklogRow `json:"rows"`
}

// WaitsView is the waits read at AsOf: the four sections and the backlog.
type WaitsView struct {
	AsOf     time.Time     `json:"as_of"`
	Section  string        `json:"section,omitempty"`
	Sections []WaitSection `json:"sections"`
	Backlog  WaitBacklog   `json:"backlog"`
}

// validWaitSection reports whether section is a section key or empty (all).
func validWaitSection(section string) bool {
	if section == "" {
		return true
	}
	for _, k := range waitSectionKeys {
		if k == section {
			return true
		}
	}
	return false
}

// ListWaits reads the four sections and the backlog at now, in one read-only
// snapshot. section filters the rows to one section ("" is every section); the
// counts always cover all four.
func (s *Store) ListWaits(ctx context.Context, now time.Time, section string) (WaitsView, error) {
	if !validWaitSection(section) {
		return WaitsView{}, taskWaitInvalid("section is not a waits section; want one of " + strings.Join(waitSectionKeys, ", ") + " or none")
	}
	now = now.UTC()
	tx, err := s.beginReportRead(ctx, true)
	if err != nil {
		return WaitsView{}, err
	}
	defer tx.Rollback(ctx)
	rows, idle, err := s.readWaitRows(ctx, tx, now)
	if err != nil {
		return WaitsView{}, err
	}
	backlog, err := readWaitBacklog(ctx, tx, rows)
	if err != nil {
		return WaitsView{}, err
	}
	view := buildWaitsView(now, section, rows, idle, backlog)
	return view, tx.Commit(ctx)
}

// readWaitRows gathers every waiting item: the derived sources, then the hand
// waits. idle is the idle_started class, which the drift report reads too.
func (s *Store) readWaitRows(ctx context.Context, tx transaction, now time.Time) ([]WaitRow, TaskDriftClass, error) {
	var rows []WaitRow
	for _, read := range []struct {
		name string
		fn   func(context.Context, transaction) ([]WaitRow, error)
	}{
		{"decision requests", readDecisionWaits},
		{"blocks", readBlockWaits},
		{"spawn start", readSpawnStartWaits},
		{"reviews", readReviewWaits},
		{"spawn failure comments", readSpawnFailureWaits},
		{"orders", readOrderWaits},
		{"hand", readHandWaits},
	} {
		got, err := read.fn(ctx, tx)
		if err != nil {
			return nil, TaskDriftClass{}, fmt.Errorf("waits %s: %w", read.name, err)
		}
		rows = append(rows, got...)
	}
	stalled := s.StartedStalledHours()
	idle, err := driftList(ctx, tx, fmt.Sprintf("researching or developing, with no activity for %d hours (started_stalled_hours)", int(stalled.Hours())), sqlFor(tx, driftIdleStartedSQL, driftIdleStartedSQLMySQL), now.Add(-stalled))
	if err != nil {
		return nil, TaskDriftClass{}, fmt.Errorf("waits idle: %w", err)
	}
	for _, t := range idle.Tasks {
		since := t.Since.UTC()
		rows = append(rows, WaitRow{
			Key: fmt.Sprintf("idle:%d", t.ID), Section: WaitSectionIdleStarted, Source: "derived:drift_idle_started",
			TaskID: t.ID, Title: t.Title, Lane: t.Lane, State: t.State,
			Reason: "no activity since " + since.Format(time.RFC3339), Since: since,
			NextAction: "check_progress", GroundsLink: taskGroundsLink(t.ID),
		})
	}
	return rows, idle, nil
}

// taskGroundsLink is the console page of one task: the grounds a row links.
func taskGroundsLink(id int64) string {
	if id < 1 {
		return ""
	}
	return fmt.Sprintf("/ui/tasks/%d", id)
}

// readDecisionWaits reads each open decision request as a wait on the operator.
// The latest answer recorded against a request is attached when it is a
// pending_confirmation with no later denial.
func readDecisionWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, `SELECT d.source_ref, COALESCE(d.task_id,0), COALESCE(t.lane,d.lane), COALESCE(t.title,d.question), COALESCE(t.project,''), COALESCE(t.state,''), d.question, d.created_at
		FROM decision_requests d LEFT JOIN tasks t ON t.id=d.task_id WHERE d.status='open' ORDER BY d.created_at, d.id`)
	if err != nil {
		return nil, err
	}
	out, err := collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		if err := r.Scan(&w.RequestID, &w.TaskID, &w.Lane, &w.Title, &w.Project, &w.State, &w.Reason, &w.Since); err != nil {
			return WaitRow{}, err
		}
		w.Key = "decision:" + w.RequestID
		w.Section = WaitSectionNeedsOperator
		w.Source = "derived:decision_request"
		w.OnKind = WaitOnOperator
		w.Since = w.Since.UTC()
		w.NextAction = "answer_decision"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		return w, nil
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	refs := make([]any, len(out))
	index := make(map[string]int, len(out))
	for i, w := range out {
		refs[i] = w.RequestID
		index[w.RequestID] = i
	}
	attempts, err := tx.Query(ctx, `SELECT request_id, result, COALESCE(text,''), actor, received_at FROM decision_answer_attempts WHERE result IN ('pending_confirmation','denied') AND request_id IN (`+inPlaceholders(tx, len(refs))+`) ORDER BY id`, refs...)
	if err != nil {
		return nil, err
	}
	latest := map[string]WaitAnswer{}
	if _, err := collectRows(attempts, func(r row) (struct{}, error) {
		var ref string
		var a WaitAnswer
		if err := r.Scan(&ref, &a.Result, &a.Words, &a.Actor, &a.At); err != nil {
			return struct{}{}, err
		}
		a.At = a.At.UTC()
		latest[ref] = a
		return struct{}{}, nil
	}); err != nil {
		return nil, err
	}
	for ref, a := range latest {
		if a.Result == DecisionResultPendingConfirmation {
			i := index[ref]
			out[i].Answer = &WaitAnswer{Result: a.Result, Words: a.Words, Actor: a.Actor, At: a.At}
			out[i].NextAction = "confirm_answer"
		}
	}
	return out, nil
}

// inPlaceholders is the placeholder list of an IN clause of n values in the
// dialect of x: $1..$n on PostgreSQL, ? on MySQL. n must be at least 1.
func inPlaceholders(x any, n int) string {
	if dialectFor(x) == mysqlDialect {
		return placeholders(n)
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(parts, ",")
}

// readBlockWaits reads each blocks edge from an unsettled task to an unsettled
// blocker: the blocked task waits on the blocker.
func readBlockWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, `SELECT t.id, b.id, t.lane, t.title, t.state, COALESCE(t.project,''), r.created_at, b.title, b.state
		FROM task_relations r JOIN tasks t ON t.id=r.dst_id JOIN tasks b ON b.id=r.src_id
		WHERE r.kind='blocks' AND r.archived_at IS NULL AND t.state NOT IN (`+settledStatesSQL+`) AND b.state NOT IN (`+settledStatesSQL+`)
		ORDER BY r.created_at, t.id, b.id`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		var blocker int64
		var blockerTitle, blockerState string
		if err := r.Scan(&w.TaskID, &blocker, &w.Lane, &w.Title, &w.State, &w.Project, &w.Since, &blockerTitle, &blockerState); err != nil {
			return WaitRow{}, err
		}
		w.Key = fmt.Sprintf("blocks:%d:%d", w.TaskID, blocker)
		w.Section = WaitSectionWaitingOn
		w.Source = "derived:blocks"
		w.OnKind = WaitOnTask
		w.OnRef = strconv.FormatInt(blocker, 10)
		w.Reason = fmt.Sprintf("blocked by task %d (%s): %s", blocker, blockerState, blockerTitle)
		w.Since = w.Since.UTC()
		w.NextAction = "wait_blocker"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		return w, nil
	})
}

// readSpawnStartWaits reads each queued spawn that waits on a start condition
// the blocking task has not reached yet.
func readSpawnStartWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	type queued struct {
		WaitRow
		until, startState string
		blocker           int64
	}
	rows, err := tx.Query(ctx, `SELECT r.task_id, r.start_task_id, r.start_until, r.updated_at, t.lane, t.title, t.state, COALESCE(t.project,''), b.state
		FROM task_spawn_records r JOIN tasks t ON t.id=r.task_id JOIN tasks b ON b.id=r.start_task_id
		WHERE r.state='waiting' AND r.start_task_id IS NOT NULL AND t.state NOT IN (`+settledStatesSQL+`)
		ORDER BY r.updated_at, r.task_id`)
	if err != nil {
		return nil, err
	}
	got, err := collectRows(rows, func(r row) (queued, error) {
		var q queued
		if err := r.Scan(&q.TaskID, &q.blocker, &q.until, &q.Since, &q.Lane, &q.Title, &q.State, &q.Project, &q.startState); err != nil {
			return queued{}, err
		}
		return q, nil
	})
	if err != nil {
		return nil, err
	}
	var out []WaitRow
	for _, q := range got {
		if spawnConditionMet(q.until, q.startState) {
			continue
		}
		w := q.WaitRow
		w.Key = fmt.Sprintf("spawn:%d", w.TaskID)
		w.Section = WaitSectionWaitingOn
		w.Source = "derived:spawn_start"
		w.OnKind = WaitOnTask
		w.OnRef = strconv.FormatInt(q.blocker, 10)
		w.Reason = fmt.Sprintf("queued spawn waits for task %d to be %s (now %s)", q.blocker, q.until, q.startState)
		w.Since = w.Since.UTC()
		w.NextAction = "wait_start_condition"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		out = append(out, w)
	}
	return out, nil
}

// readReviewWaits reads each open change linked to an unsettled task as a wait
// on its PR.
func readReviewWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, `SELECT ct.task_id, c.repo, c.pr_number, c.opened_at, t.lane, t.title, t.state, COALESCE(t.project,'')
		FROM change_tasks ct JOIN changes c ON c.id=ct.change_id JOIN tasks t ON t.id=ct.task_id
		WHERE c.state='open' AND t.state NOT IN (`+settledStatesSQL+`)
		ORDER BY c.opened_at, c.id, ct.task_id`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		var repo string
		var number int64
		if err := r.Scan(&w.TaskID, &repo, &number, &w.Since, &w.Lane, &w.Title, &w.State, &w.Project); err != nil {
			return WaitRow{}, err
		}
		ref := fmt.Sprintf("%s#%d", repo, number)
		w.Key = fmt.Sprintf("review:%d:%s", w.TaskID, ref)
		w.Section = WaitSectionWaitingOn
		w.Source = "derived:change_review"
		w.OnKind = WaitOnPR
		w.OnRef = ref
		w.Reason = "PR open in review: " + ref
		w.Since = w.Since.UTC()
		w.NextAction = "wait_review"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		return w, nil
	})
}

// readHandWaits reads the open hand waits. A hand wait on the operator is in
// needs_operator, a failed start (external:spawn) in failed_start, and any other
// hand wait in waiting_on.
func readHandWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, `SELECT w.id, w.task_id, w.on_kind, w.on_ref, w.reason, w.since, w.source, t.lane, t.title, t.state, COALESCE(t.project,'')
		FROM task_waits w JOIN tasks t ON t.id=w.task_id WHERE w.cleared_at IS NULL ORDER BY w.since, w.id`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		var waitID int64
		if err := r.Scan(&waitID, &w.TaskID, &w.OnKind, &w.OnRef, &w.Reason, &w.Since, &w.Source, &w.Lane, &w.Title, &w.State, &w.Project); err != nil {
			return WaitRow{}, err
		}
		w.Key = fmt.Sprintf("wait:%d", waitID)
		w.Since = w.Since.UTC()
		w.GroundsLink = taskGroundsLink(w.TaskID)
		switch {
		case w.OnKind == WaitOnOperator:
			w.Section, w.NextAction = WaitSectionNeedsOperator, "operator_step"
		case w.OnKind == WaitOnExternal && w.OnRef == WaitRefSpawn:
			w.Section, w.NextAction = WaitSectionFailedStart, "retry_spawn"
		default:
			w.Section, w.NextAction = WaitSectionWaitingOn, "wait_"+w.OnKind
		}
		return w, nil
	})
}

// readWaitBacklog lists the ordinary backlog: backlog tasks that no section row
// names. It reads the first waitBacklogScan backlog tasks and lists the first
// waitBacklogRows of those left over.
func readWaitBacklog(ctx context.Context, tx transaction, rows []WaitRow) (WaitBacklog, error) {
	inSection := map[int64]bool{}
	for _, r := range rows {
		inSection[r.TaskID] = true
	}
	found, err := tx.Query(ctx, `SELECT id, lane, title, COALESCE(project,''), created_at FROM tasks WHERE state='backlog' ORDER BY created_at, id LIMIT `+strconv.Itoa(waitBacklogScan))
	if err != nil {
		return WaitBacklog{}, err
	}
	all, err := collectRows(found, func(r row) (WaitBacklogRow, error) {
		var b WaitBacklogRow
		if err := r.Scan(&b.TaskID, &b.Lane, &b.Title, &b.Project, &b.CreatedAt); err != nil {
			return WaitBacklogRow{}, err
		}
		b.CreatedAt = b.CreatedAt.UTC()
		return b, nil
	})
	if err != nil {
		return WaitBacklog{}, err
	}
	out := WaitBacklog{Rows: []WaitBacklogRow{}, Truncated: len(all) == waitBacklogScan}
	for _, b := range all {
		if inSection[b.TaskID] {
			continue
		}
		out.Count++
		if len(out.Rows) < waitBacklogRows {
			out.Rows = append(out.Rows, b)
		}
	}
	return out, nil
}

// buildWaitsView sorts the rows into the four sections, oldest first, and sets
// each row's age at now. A section filter keeps only that section's rows.
func buildWaitsView(now time.Time, section string, rows []WaitRow, idle TaskDriftClass, backlog WaitBacklog) WaitsView {
	bySection := map[string][]WaitRow{}
	for _, r := range rows {
		r.AgeSeconds = waitAgeSeconds(now, r.Since)
		bySection[r.Section] = append(bySection[r.Section], r)
	}
	view := WaitsView{AsOf: now, Section: section, Sections: make([]WaitSection, 0, len(waitSectionKeys)), Backlog: backlog}
	if section != "" {
		view.Backlog.Rows = []WaitBacklogRow{}
	}
	for _, key := range waitSectionKeys {
		xs := bySection[key]
		sort.SliceStable(xs, func(i, j int) bool {
			if !xs[i].Since.Equal(xs[j].Since) {
				return xs[i].Since.Before(xs[j].Since)
			}
			return xs[i].Key < xs[j].Key
		})
		sec := WaitSection{Key: key, Count: len(xs), Rows: []WaitRow{}}
		if key == WaitSectionIdleStarted {
			sec.Truncated = idle.Truncated
		}
		if section == "" || section == key {
			sec.Rows = xs
		}
		view.Sections = append(view.Sections, sec)
	}
	return view
}

// waitAgeSeconds is how long a wait has run at now, never negative.
func waitAgeSeconds(now, since time.Time) int64 {
	if d := now.Sub(since); d > 0 {
		return int64(d / time.Second)
	}
	return 0
}

// spawnFailedPrefix opens a spawn failure comment. panewire posts it for a spawn
// that ends failed, refused or deadline_exceeded; a session may write the same
// form by hand.
const spawnFailedPrefix = "spawn failed request="

// spawnFailureSQL reads each task whose latest spawn failure comment is not
// followed by a start: a transition into a started state after the comment ends
// the failed start. toCol is the dialect's quoted "to" column of task_events.
func spawnFailureSQL(toCol string) string {
	return `SELECT c.id, c.task_id, c.body, c.created_at, t.lane, t.title, t.state, COALESCE(t.project,'')
		FROM task_comments c JOIN tasks t ON t.id=c.task_id
		WHERE c.body LIKE '` + spawnFailedPrefix + `%' AND t.state NOT IN (` + settledStatesSQL + `)
		AND c.id = (SELECT max(c2.id) FROM task_comments c2 WHERE c2.task_id=c.task_id AND c2.body LIKE '` + spawnFailedPrefix + `%')
		AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id=c.task_id AND e.kind='transition' AND e.at > c.created_at AND ` + toCol + ` IN (` + startedStatesSQL + `))
		ORDER BY c.created_at, c.id`
}

// readSpawnFailureWaits reads a failed start from its latest "spawn failed"
// comment. It is an external:spawn wait in failed_start, the same section as a
// session's hand wait on that failure; the hand wait stays as it is.
func readSpawnFailureWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, sqlFor(tx, spawnFailureSQL(`e."to"`), spawnFailureSQL("e.`to`")))
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		var commentID int64
		var body string
		if err := r.Scan(&commentID, &w.TaskID, &body, &w.Since, &w.Lane, &w.Title, &w.State, &w.Project); err != nil {
			return WaitRow{}, err
		}
		line, _, _ := strings.Cut(body, "\n")
		w.Key = fmt.Sprintf("spawn-failed:%d", commentID)
		w.Section = WaitSectionFailedStart
		w.Source = "derived:spawn_failed_comment"
		w.OnKind = WaitOnExternal
		w.OnRef = WaitRefSpawn
		w.Reason = line
		w.Since = w.Since.UTC()
		w.NextAction = "retry_spawn"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		return w, nil
	})
}

// readOrderWaits reads each open order (neither acknowledged nor withdrawn) as a
// wait on its target lane: the order waits until that lane acknowledges it (hk
// 5125, design hk 5112 §5 and §8.1).
func readOrderWaits(ctx context.Context, tx transaction) ([]WaitRow, error) {
	rows, err := tx.Query(ctx, `SELECT o.id, o.task_id, o.target_lane, o.created_at, t.lane, t.title, t.state, COALESCE(t.project,'')
		FROM task_orders o JOIN tasks t ON t.id=o.task_id
		WHERE o.acked_at IS NULL AND o.withdrawn_at IS NULL ORDER BY o.created_at, o.id`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (WaitRow, error) {
		var w WaitRow
		var orderID string
		if err := r.Scan(&orderID, &w.TaskID, &w.OnRef, &w.Since, &w.Lane, &w.Title, &w.State, &w.Project); err != nil {
			return WaitRow{}, err
		}
		w.Key = "order:" + orderID
		w.Section = WaitSectionWaitingOn
		w.Source = "derived:task_order"
		w.OnKind = WaitOnLane
		w.Reason = "order " + orderID + " not yet acknowledged"
		w.Since = w.Since.UTC()
		w.NextAction = "wait_lane"
		w.GroundsLink = taskGroundsLink(w.TaskID)
		return w, nil
	})
}

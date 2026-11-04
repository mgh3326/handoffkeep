package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// The desk ledger (docs/linear-internalize.md, "Desk ledger") is one
// server-computed view of what the desk must know after a restart. Its five
// sections are fixed and ordered:
//
//  1. In flight: started tasks that are neither flagged nor stalled.
//  2. Waiting on operator: open, not snoozed decision requests, grouped by
//     task.
//  3. Stalled: stalled started tasks, and recent source events that rule 4
//     says need a look (a merge event for a canceled task).
//  4. Triage: tasks in triage, oldest first.
//  5. Changed since last ack: the change-feed delta from the consumer's
//     cursor. There is no second feed: the section reads sync_log through
//     syncDeltaTx, with the feed's reset rule.
//
// A task appears in at most one of the first three. A task waiting on a
// decision (the awaiting_decision flag, or any open decision request) is
// listed only under Waiting, and a stalled task only under Stalled.
//
// Reading never moves a cursor; only AckLedger does. Everything is read in one
// repeatable-read snapshot and every list ends its order in a unique id, so
// the same data read at the same now gives the same ledger.

const (
	// LedgerDeltaRows is how many feed rows section 5 lists; its summary
	// counts every row after the cursor.
	LedgerDeltaRows = 100
	// LedgerEventWindow is how far back section 3 looks for events recorded
	// with applied=false.
	LedgerEventWindow = 7 * 24 * time.Hour
	// ledgerListLimit bounds the triage and event lists; counts are exact.
	ledgerListLimit = 500
)

// ErrLedgerAckBackward refuses an ack below the consumer's cursor: a cursor
// only moves forward.
var ErrLedgerAckBackward = errors.New("ledger_ack_backward")

// LedgerTask is the part of a task the ledger shows.
type LedgerTask struct {
	ID        int64   `json:"id"`
	Lane      string  `json:"lane"`
	Title     string  `json:"title"`
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	Priority  int     `json:"priority"`
	Project   *string `json:"project"`
	ClaimedBy string  `json:"claimed_by,omitempty"`
	PR        string  `json:"pr,omitempty"`
	JobID     string  `json:"job_id,omitempty"`
}

// LedgerEvent is a task's newest task_events row.
type LedgerEvent struct {
	Kind   string    `json:"kind"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// LedgerStartedTask is a started task under In flight or Stalled. ActivityAt
// is the activity the stalled rule compares (taskActivitySQL). NextTrigger
// names the event that moves the task next and NextSource who sends it.
type LedgerStartedTask struct {
	LedgerTask
	ActivityAt  time.Time    `json:"activity_at"`
	LastEvent   *LedgerEvent `json:"last_event"`
	NextTrigger string       `json:"next_trigger"`
	NextSource  string       `json:"next_source"`
}

// LedgerProjectStatus is one project with started tasks, in the In flight
// header: its latest status update and whether that update is stale.
type LedgerProjectStatus struct {
	Project      string     `json:"project"`
	Started      int        `json:"started"`
	Health       string     `json:"health"`
	LastUpdateAt *time.Time `json:"last_update_at"`
	Stale        bool       `json:"stale"`
}

type LedgerInFlight struct {
	Projects []LedgerProjectStatus `json:"projects"`
	Tasks    []LedgerStartedTask   `json:"tasks"`
}

// LedgerDecisionGroup is the open requests of one task, or (TaskID nil) the
// requests that name no task. Task is nil when the named task does not
// exist.
type LedgerDecisionGroup struct {
	TaskID   *int64           `json:"task_id"`
	Task     *LedgerTask      `json:"task"`
	Requests []DecisionRecord `json:"requests"`
}

type LedgerWaiting struct {
	Count  int                   `json:"count"`
	Groups []LedgerDecisionGroup `json:"groups"`
}

// LedgerFlaggedEvent is a source event recorded with applied=false that needs
// a look: a merge event for a canceled task (rule 4).
type LedgerFlaggedEvent struct {
	EventID   int64      `json:"event_id"`
	Task      LedgerTask `json:"task"`
	Source    string     `json:"source"`
	SourceRef string     `json:"source_ref"`
	Note      string     `json:"note"`
	At        time.Time  `json:"at"`
}

type LedgerStalled struct {
	Tasks  []LedgerStartedTask  `json:"tasks"`
	Events []LedgerFlaggedEvent `json:"events"`
}

// LedgerTriageTask is a task in triage. Since is when it entered triage
// (started_triage_at, or created_at on rows without it).
type LedgerTriageTask struct {
	LedgerTask
	Since      time.Time `json:"since"`
	AgeSeconds int64     `json:"age_seconds"`
}

type LedgerTriage struct {
	Count            int                `json:"count"`
	OldestAt         *time.Time         `json:"oldest_at"`
	OldestAgeSeconds int64              `json:"oldest_age_seconds"`
	Tasks            []LedgerTriageTask `json:"tasks"`
}

// LedgerCursor is one consumer's read position in the change feed.
type LedgerCursor struct {
	Consumer    string    `json:"consumer"`
	AckedSyncID int64     `json:"acked_sync_id"`
	AckedAt     time.Time `json:"acked_at"`
	UpdatedBy   string    `json:"updated_by"`
}

type LedgerChangeCount struct {
	Model  string `json:"model"`
	Action string `json:"action"`
	Count  int64  `json:"count"`
}

// LedgerChanges is section 5. With no cursor yet, NoCursor is set and the
// delta is empty. Otherwise Rows are the first LedgerDeltaRows feed rows after
// the cursor, Summary counts every row after it by model and action, and
// LastSyncID is the last listed row (the cursor when none is listed).
type LedgerChanges struct {
	NoCursor   bool                `json:"no_cursor"`
	Cursor     *LedgerCursor       `json:"cursor"`
	MaxSyncID  int64               `json:"max_sync_id"`
	LastSyncID int64               `json:"last_sync_id"`
	Total      int64               `json:"total"`
	Truncated  bool                `json:"truncated"`
	Summary    []LedgerChangeCount `json:"summary"`
	Rows       []SyncChange        `json:"rows"`
}

// Ledger is the whole view. Field order is the section order.
type Ledger struct {
	Consumer          string         `json:"consumer"`
	At                time.Time      `json:"at"`
	StalledAfterHours int64          `json:"stalled_after_hours"`
	InFlight          LedgerInFlight `json:"in_flight"`
	WaitingOnOperator LedgerWaiting  `json:"waiting_on_operator"`
	Stalled           LedgerStalled  `json:"stalled"`
	Triage            LedgerTriage   `json:"triage"`
	ChangedSinceAck   LedgerChanges  `json:"changed_since_ack"`
}

// ledgerStartedRow is a started task as read, before it is placed.
// Flagged is the awaiting_decision flag or any open decision request.
type ledgerStartedRow struct {
	LedgerStartedTask
	Flagged bool
}

// ledgerNextTrigger is what moves a task next, from its state and refs:
// a PR or review event moves researching and developing to reviewing, a merge
// moves reviewing to merged (by hand when no PR is recorded), and a deploy
// record moves a merged task with a PR to deployed.
func ledgerNextTrigger(state, pr string) (trigger, source string) {
	switch state {
	case StateResearching, StateDeveloping:
		return "review", SourceMergewakePR
	case StateReviewing:
		if pr == "" {
			return "merge", "manual"
		}
		return "merge", SourceMergewakeMerge
	case StateMerged:
		if pr != "" {
			return "deploy", SourceDeploy
		}
	}
	return "", ""
}

// ledgerSections places started tasks, decision requests and flagged events
// into the first three sections. records are open, not snoozed requests,
// oldest first; tasks names the tasks they point at. A task with any open
// request (snoozed included) or the awaiting_decision flag is in neither In
// flight nor Stalled; one with a listed request is under Waiting.
func ledgerSections(started []ledgerStartedRow, records []DecisionRecord, tasks map[int64]LedgerTask, events []LedgerFlaggedEvent, now time.Time, stalledAfter time.Duration) (LedgerInFlight, LedgerWaiting, LedgerStalled) {
	inFlight := LedgerInFlight{Projects: []LedgerProjectStatus{}, Tasks: []LedgerStartedTask{}}
	waiting := LedgerWaiting{Count: len(records), Groups: []LedgerDecisionGroup{}}
	stalled := LedgerStalled{Tasks: []LedgerStartedTask{}, Events: []LedgerFlaggedEvent{}}

	waitingTask := map[int64]bool{}
	group := map[int64]int{}
	noTask := -1
	for _, r := range records {
		if r.TaskID == nil {
			if noTask < 0 {
				waiting.Groups = append(waiting.Groups, LedgerDecisionGroup{Requests: []DecisionRecord{}})
				noTask = len(waiting.Groups) - 1
			}
			waiting.Groups[noTask].Requests = append(waiting.Groups[noTask].Requests, r)
			continue
		}
		id := *r.TaskID
		waitingTask[id] = true
		i, ok := group[id]
		if !ok {
			g := LedgerDecisionGroup{TaskID: &id, Requests: []DecisionRecord{}}
			if x, found := tasks[id]; found {
				g.Task = &x
			}
			waiting.Groups = append(waiting.Groups, g)
			i = len(waiting.Groups) - 1
			group[id] = i
		}
		waiting.Groups[i].Requests = append(waiting.Groups[i].Requests, r)
	}
	// Groups are in the order of their oldest request; the group of requests
	// with no task goes last.
	if noTask >= 0 && noTask != len(waiting.Groups)-1 {
		g := waiting.Groups[noTask]
		waiting.Groups = append(append(waiting.Groups[:noTask:noTask], waiting.Groups[noTask+1:]...), g)
	}

	for _, r := range started {
		if r.Flagged || waitingTask[r.ID] {
			continue
		}
		x := r.LedgerStartedTask
		x.NextTrigger, x.NextSource = ledgerNextTrigger(x.State, x.PR)
		if isStalled(x.State, false, x.ActivityAt, now, stalledAfter) {
			stalled.Tasks = append(stalled.Tasks, x)
		} else {
			inFlight.Tasks = append(inFlight.Tasks, x)
		}
	}
	sort.SliceStable(inFlight.Tasks, func(i, j int) bool { return inFlight.Tasks[i].ID < inFlight.Tasks[j].ID })
	sort.SliceStable(stalled.Tasks, func(i, j int) bool {
		a, b := stalled.Tasks[i], stalled.Tasks[j]
		if !a.ActivityAt.Equal(b.ActivityAt) {
			return a.ActivityAt.Before(b.ActivityAt)
		}
		return a.ID < b.ID
	})
	for _, e := range events {
		if !waitingTask[e.Task.ID] {
			stalled.Events = append(stalled.Events, e)
		}
	}
	return inFlight, waiting, stalled
}

// statusLatest is a project's newest status update.
type statusLatest struct {
	Health string
	At     time.Time
}

// ledgerProjects is the In flight header: every project with started tasks,
// by name, with its latest status update and staleness at now.
func ledgerProjects(started []ledgerStartedRow, latest map[string]statusLatest, now time.Time) []LedgerProjectStatus {
	counts := map[string]int{}
	for _, r := range started {
		if r.Project != nil && *r.Project != "" {
			counts[*r.Project]++
		}
	}
	out := make([]LedgerProjectStatus, 0, len(counts))
	for name, n := range counts {
		p := LedgerProjectStatus{Project: name, Started: n}
		if u, ok := latest[name]; ok {
			at := u.At
			p.Health, p.LastUpdateAt = u.Health, &at
		}
		p.Stale = StatusUpdateStale(p.LastUpdateAt, now)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out
}

// ledgerTaskColumns reads a LedgerTask from the tasks row named alias.
func ledgerTaskColumns(alias string) string {
	return fmt.Sprintf(`%[1]s.id,%[1]s.lane,%[1]s.title,%[1]s.kind,%[1]s.state,%[1]s.priority,%[1]s.project,%[1]s.claimed_by,COALESCE(%[1]s.refs->>'pr',''),COALESCE(%[1]s.refs->>'job_id','')`, alias)
}

func ledgerTaskDest(x *LedgerTask) []any {
	return []any{&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.Priority, &x.Project, &x.ClaimedBy, &x.PR, &x.JobID}
}

// Ledger reads the desk ledger for consumer at now. It writes nothing; a
// cursor older than the feed's reset boundary returns *SyncReset.
func (s *Store) Ledger(ctx context.Context, consumer string, now time.Time) (Ledger, error) {
	if !validName(consumer) {
		return Ledger{}, errors.New("invalid ledger consumer")
	}
	now = now.UTC()
	after := s.StartedStalledHours()
	out := Ledger{Consumer: consumer, At: now, StalledAfterHours: int64(after / time.Hour)}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Ledger{}, err
	}
	defer tx.Rollback(ctx)

	// Section 5 first: a cursor past the reset boundary answers before any
	// other read.
	if out.ChangedSinceAck, err = ledgerChangesTx(ctx, tx, consumer); err != nil {
		return Ledger{}, err
	}
	started, err := ledgerStartedTx(ctx, tx)
	if err != nil {
		return Ledger{}, err
	}
	records, err := listDecisionRecords(ctx, tx, DecisionRecordFilter{Status: DecisionStatusOpen}, now)
	if err != nil {
		return Ledger{}, err
	}
	ids := []int64{}
	for _, r := range records {
		if r.TaskID != nil {
			ids = append(ids, *r.TaskID)
		}
	}
	tasks, err := ledgerTasksTx(ctx, tx, ids)
	if err != nil {
		return Ledger{}, err
	}
	events, err := ledgerFlaggedEventsTx(ctx, tx, now.Add(-LedgerEventWindow))
	if err != nil {
		return Ledger{}, err
	}
	out.InFlight, out.WaitingOnOperator, out.Stalled = ledgerSections(started, records, tasks, events, now, after)

	projects := []string{}
	seen := map[string]bool{}
	for _, r := range started {
		if r.Project != nil && !seen[*r.Project] {
			seen[*r.Project] = true
			projects = append(projects, *r.Project)
		}
	}
	latest, err := latestStatusUpdatesTx(ctx, tx, projects)
	if err != nil {
		return Ledger{}, err
	}
	out.InFlight.Projects = ledgerProjects(started, latest, now)

	if out.Triage, err = ledgerTriageTx(ctx, tx, now); err != nil {
		return Ledger{}, err
	}
	return out, tx.Commit(ctx)
}

// ledgerStartedTx reads every started task with its activity, its newest
// event and whether it waits on a decision.
func ledgerStartedTx(ctx context.Context, tx pgx.Tx) ([]ledgerStartedRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+ledgerTaskColumns("tasks")+`,
		tasks.awaiting_decision OR EXISTS(SELECT 1 FROM decision_requests d WHERE d.task_id=tasks.id AND d.status='open'),
		`+taskActivitySQL+`,
		le.kind,le.source,le.at
		FROM tasks LEFT JOIN LATERAL (SELECT e.kind,e.source,e.at FROM task_events e WHERE e.task_id=tasks.id ORDER BY e.at DESC,e.id DESC LIMIT 1) le ON true
		WHERE tasks.state IN (`+startedStatesSQL+`)
		ORDER BY tasks.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ledgerStartedRow{}
	for rows.Next() {
		var r ledgerStartedRow
		var kind, source *string
		var at *time.Time
		dest := append(ledgerTaskDest(&r.LedgerTask), &r.Flagged, &r.ActivityAt, &kind, &source, &at)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.ActivityAt = r.ActivityAt.UTC()
		if kind != nil && at != nil {
			r.LastEvent = &LedgerEvent{Kind: *kind, At: at.UTC()}
			if source != nil {
				r.LastEvent.Source = *source
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ledgerTasksTx reads the tasks named by ids.
func ledgerTasksTx(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]LedgerTask, error) {
	out := map[int64]LedgerTask{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+ledgerTaskColumns("t")+` FROM tasks t WHERE t.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x LedgerTask
		if err := rows.Scan(ledgerTaskDest(&x)...); err != nil {
			return nil, err
		}
		out[x.ID] = x
	}
	return out, rows.Err()
}

// ledgerFlaggedEventsTx reads rule 4's events since since: a merge event
// recorded with applied=false because its task was canceled, for a task that
// is still canceled. Oldest first.
func ledgerFlaggedEventsTx(ctx context.Context, tx pgx.Tx, since time.Time) ([]LedgerFlaggedEvent, error) {
	rows, err := tx.Query(ctx, `SELECT e.id,e.source,e.source_ref,e.note,e.at,`+ledgerTaskColumns("t")+`
		FROM task_events e JOIN tasks t ON t.id=e.task_id
		WHERE e.kind=$1 AND NOT e.applied AND e.source=$2 AND e.at>=$3
		AND e."from" IN ('dropped','duplicate','icebox') AND t.state IN ('dropped','duplicate','icebox')
		ORDER BY e.at,e.id LIMIT $4`, TaskEventSource, SourceMergewakeMerge, since, ledgerListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LedgerFlaggedEvent{}
	for rows.Next() {
		var e LedgerFlaggedEvent
		dest := append([]any{&e.EventID, &e.Source, &e.SourceRef, &e.Note, &e.At}, ledgerTaskDest(&e.Task)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ledgerTriageTx reads the triage queue, oldest first.
func ledgerTriageTx(ctx context.Context, tx pgx.Tx, now time.Time) (LedgerTriage, error) {
	out := LedgerTriage{Tasks: []LedgerTriageTask{}}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE state=$1`, StateTriage).Scan(&out.Count); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+ledgerTaskColumns("t")+`,COALESCE(t.started_triage_at,t.created_at) AS since
		FROM tasks t WHERE t.state=$1 ORDER BY since,t.id LIMIT $2`, StateTriage, ledgerListLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var x LedgerTriageTask
		if err := rows.Scan(append(ledgerTaskDest(&x.LedgerTask), &x.Since)...); err != nil {
			return out, err
		}
		x.Since = x.Since.UTC()
		x.AgeSeconds = int64(now.Sub(x.Since) / time.Second)
		out.Tasks = append(out.Tasks, x)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Tasks) > 0 {
		oldest := out.Tasks[0]
		out.OldestAt, out.OldestAgeSeconds = &oldest.Since, oldest.AgeSeconds
	}
	return out, nil
}

// ledgerChangesTx reads section 5 from the consumer's cursor.
func ledgerChangesTx(ctx context.Context, tx pgx.Tx, consumer string) (LedgerChanges, error) {
	out := LedgerChanges{Summary: []LedgerChangeCount{}, Rows: []SyncChange{}}
	var c LedgerCursor
	err := tx.QueryRow(ctx, `SELECT consumer,acked_sync_id,acked_at,updated_by FROM ledger_cursors WHERE consumer=$1`, consumer).Scan(&c.Consumer, &c.AckedSyncID, &c.AckedAt, &c.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		out.NoCursor = true
		return out, tx.QueryRow(ctx, `SELECT max_sync_id FROM sync_state WHERE singleton`).Scan(&out.MaxSyncID)
	}
	if err != nil {
		return out, err
	}
	c.AckedAt = c.AckedAt.UTC()
	out.Cursor = &c
	rows, meta, err := syncDeltaTx(ctx, tx, c.AckedSyncID, LedgerDeltaRows)
	if err != nil {
		return out, err
	}
	out.Rows, out.MaxSyncID, out.LastSyncID = rows, meta.MaxSyncID, meta.LastSyncID
	summary, err := tx.Query(ctx, `SELECT model,action,count(*) FROM sync_log
		WHERE sync_id IS NOT NULL AND sync_id>$1 AND sync_id<=$2
		GROUP BY model,action ORDER BY model,action`, c.AckedSyncID, meta.MaxSyncID)
	if err != nil {
		return out, err
	}
	defer summary.Close()
	for summary.Next() {
		var x LedgerChangeCount
		if err := summary.Scan(&x.Model, &x.Action, &x.Count); err != nil {
			return out, err
		}
		out.Summary = append(out.Summary, x)
		out.Total += x.Count
	}
	if err := summary.Err(); err != nil {
		return out, err
	}
	out.Truncated = out.Total > int64(len(out.Rows))
	return out, nil
}

// AckLedger moves consumer's cursor to syncID. A cursor only moves forward: a
// lower syncID is refused with ErrLedgerAckBackward, and the same one changes
// only acked_at. syncID past the feed watermark is invalid, and one below the
// feed's reset boundary returns *SyncReset. An unknown consumer's first ack
// creates its cursor at the current watermark, whatever valid syncID it
// names: before that ack it has seen no delta to acknowledge.
//
// Lock order (lockorder.go): the transaction locks one ledger_cursors row and
// reads sync_state without a lock.
func (s *Store) AckLedger(ctx context.Context, consumer string, syncID int64, by string, now time.Time) (LedgerCursor, error) {
	if !validName(consumer) || syncID < 0 || by == "" || !validText(by, 128) {
		return LedgerCursor{}, errors.New("invalid ledger ack")
	}
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LedgerCursor{}, err
	}
	defer tx.Rollback(ctx)
	var maxID, floor int64
	if err := tx.QueryRow(ctx, `SELECT max_sync_id,pruned_through FROM sync_state WHERE singleton`).Scan(&maxID, &floor); err != nil {
		return LedgerCursor{}, err
	}
	if syncID > maxID {
		return LedgerCursor{}, ErrInvalidSync
	}
	const cursorColumns = `consumer,acked_sync_id,acked_at,updated_by`
	scan := func(row pgx.Row) (LedgerCursor, error) {
		var c LedgerCursor
		err := row.Scan(&c.Consumer, &c.AckedSyncID, &c.AckedAt, &c.UpdatedBy)
		c.AckedAt = c.AckedAt.UTC()
		return c, err
	}
	c, err := scan(tx.QueryRow(ctx, `SELECT `+cursorColumns+` FROM ledger_cursors WHERE consumer=$1 FOR UPDATE`, consumer))
	if errors.Is(err, pgx.ErrNoRows) {
		c, err = scan(tx.QueryRow(ctx, `INSERT INTO ledger_cursors(consumer,acked_sync_id,acked_at,updated_by) VALUES($1,$2,$3,$4)
			ON CONFLICT (consumer) DO NOTHING RETURNING `+cursorColumns, consumer, maxID, now, by))
		if err == nil {
			return c, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return LedgerCursor{}, err
		}
		// A concurrent first ack created the row; this ack applies to it.
		c, err = scan(tx.QueryRow(ctx, `SELECT `+cursorColumns+` FROM ledger_cursors WHERE consumer=$1 FOR UPDATE`, consumer))
	}
	if err != nil {
		return LedgerCursor{}, err
	}
	if syncID < c.AckedSyncID {
		return LedgerCursor{}, fmt.Errorf("%w: the cursor is at %d", ErrLedgerAckBackward, c.AckedSyncID)
	}
	if syncID < floor {
		minID := maxID + 1
		if err := tx.QueryRow(ctx, `SELECT COALESCE(min(sync_id),$1) FROM sync_log`, minID).Scan(&minID); err != nil {
			return LedgerCursor{}, err
		}
		return LedgerCursor{}, &SyncReset{MinSyncID: minID}
	}
	c, err = scan(tx.QueryRow(ctx, `UPDATE ledger_cursors SET acked_sync_id=$2,acked_at=$3,updated_by=$4 WHERE consumer=$1 RETURNING `+cursorColumns, consumer, syncID, now, by))
	if err != nil {
		return LedgerCursor{}, err
	}
	return c, tx.Commit(ctx)
}

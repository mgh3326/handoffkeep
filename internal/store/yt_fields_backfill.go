package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The issue_fields backfill (hk 4022, docs/youtrack-write-protocol.md "Kind
// issue_fields", Backfill). The task_fields consumer sends a task's snapshot
// only on the task's next change after its cursor, so a bound task that has
// not changed since the import ended keeps the import's fields. `youtrack
// fields-backfill` walks the tasks once and prepares each one's snapshot
// through prepareYTFieldsTx, the consumer's rule: an unchanged snapshot (the
// sha last prepared) prepares nothing, so a second run is a no-op. It only
// prepares; the write worker sends the ops and paces them on YouTrack's 429s.

// ErrYTFieldsOff refuses a backfill while the fields mode is off: the worker
// would admit none of its ops.
var ErrYTFieldsOff = errors.New("yt_fields_off")

// YTFieldsBackfillOptions selects the tasks of one backfill run. Project and
// TaskID narrow the walk ("" and 0 for every task); Limit caps the ops
// prepared in this run (0 for no cap; a moved task makes two ops, so a cap of
// 1 cannot prepare one); DryRun counts without writing.
type YTFieldsBackfillOptions struct {
	Project string
	TaskID  int64
	Limit   int
	DryRun  bool
}

// YTFieldsBackfillCounts is one class count per outcome of the consumer's
// rule. Scanned counts the tasks read; Prepared the tasks whose snapshot op was
// made in place (or, in a dry run, would be); Ops every op made, issue_move
// and issue_fields alike (a moved task makes two), which --limit caps.
type YTFieldsBackfillCounts struct {
	Scanned     int `json:"scanned"`
	Ops         int `json:"ops"`
	Prepared    int `json:"prepared"`
	Unchanged   int `json:"unchanged"`
	Unbound     int `json:"unbound"`
	Moved       int `json:"moved"`
	MoveSkipped int `json:"move_skipped"`
	OtherTenant int `json:"other_tenant"`
	Invalid     int `json:"invalid"`
}

func (c *YTFieldsBackfillCounts) add(o ytFieldsOutcome, ops int) {
	c.Scanned++
	c.Ops += ops
	switch o {
	case ytFieldsPrepared:
		c.Prepared++
	case ytFieldsUnchanged:
		c.Unchanged++
	case ytFieldsUnbound:
		c.Unbound++
	case ytFieldsMoved:
		c.Moved++
	case ytFieldsMoveSkipped:
		c.MoveSkipped++
	case ytFieldsOtherTenant:
		c.OtherTenant++
	case ytFieldsInvalid:
		c.Invalid++
	}
}

// YTFieldsBackfillProject is the counts of one hk project ("" for tasks with
// none).
type YTFieldsBackfillProject struct {
	Project string `json:"project"`
	YTFieldsBackfillCounts
}

// YTFieldsBackfillReport is what `youtrack fields-backfill` did, per project
// in order of each project's first task id, and in total. OverLimit is the
// tasks this run left for a later one because their ops did not fit what was
// left of Limit; they are in no count.
type YTFieldsBackfillReport struct {
	Tenant   string                     `json:"tenant"`
	Project  string                     `json:"project,omitempty"`
	TaskID   int64                      `json:"task_id,omitempty"`
	Limit    int                        `json:"limit,omitempty"`
	DryRun   bool                       `json:"dry_run"`
	Projects []*YTFieldsBackfillProject `json:"projects"`
	Total    YTFieldsBackfillCounts     `json:"total"`
	// OverLimit makes a run that prepared nothing explain itself: with Limit 1
	// every moved task is over the limit.
	OverLimit int `json:"over_limit,omitempty"`
}

// BackfillYTFieldsOps prepares the issue_fields snapshot of every task opt
// selects, in id order, each in its own transaction under the task's lane
// lock, by the consumer's rule and the consumer's gate (mode on and its
// tenant; off is ErrYTFieldsOff). It stops once opt.Limit ops are prepared,
// counting the issue_move ops with the issue_fields ones (hk 4112). A task
// whose ops do not fit what is left of the limit is skipped for this run
// (counted in OverLimit) and the walk goes on, so a task that fits still runs
// behind it and a run may stop one op short of the limit. The next run starts
// again from the first task and passes the ones already sent as unchanged. A
// dry run reads the same rows and writes nothing.
func (s *Store) BackfillYTFieldsOps(ctx context.Context, opt YTFieldsBackfillOptions) (YTFieldsBackfillReport, error) {
	r := YTFieldsBackfillReport{Project: opt.Project, TaskID: opt.TaskID, Limit: opt.Limit, DryRun: opt.DryRun,
		Projects: []*YTFieldsBackfillProject{}}
	mode, tenant := s.ytFieldsNow()
	if mode != YTFieldsOn {
		return r, ErrYTFieldsOff
	}
	r.Tenant = tenant
	if opt.Limit < 0 || opt.TaskID < 0 {
		return r, errors.New("backfill limit and task id must not be negative")
	}
	query, args := sqlFor(s, `SELECT id FROM tasks WHERE ($1 = '' OR project = $1) AND ($2 = 0 OR id = $2) ORDER BY id`,
		`SELECT id FROM tasks WHERE (? = '' OR project = ?) AND (? = 0 OR id = ?) ORDER BY id`),
		[]any{opt.Project, opt.TaskID}
	if dialectFor(s) == mysqlDialect {
		args = []any{opt.Project, opt.Project, opt.TaskID, opt.TaskID}
	}
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return r, err
	}
	ids, err := collectRows(rows, rowTo[int64])
	if err != nil {
		return r, err
	}
	byProject := map[string]*YTFieldsBackfillProject{}
	for _, id := range ids {
		if opt.Limit > 0 && r.Total.Ops >= opt.Limit {
			break
		}
		remaining := 0
		if opt.Limit > 0 {
			remaining = opt.Limit - r.Total.Ops
		}
		outcome, ops, project, overLimit, err := s.backfillYTFields(ctx, tenant, id, opt.DryRun, remaining)
		if err != nil {
			return r, fmt.Errorf("task %d: %w", id, err)
		}
		if overLimit {
			// Not this run's: it needs more ops than are left. A later task that
			// needs fewer still fits, so go on rather than stall behind it.
			r.OverLimit++
			continue
		}
		if outcome == ytFieldsDeleted {
			continue
		}
		p := byProject[project]
		if p == nil {
			p = &YTFieldsBackfillProject{Project: project}
			byProject[project] = p
			r.Projects = append(r.Projects, p)
		}
		p.add(outcome, ops)
		r.Total.add(outcome, ops)
	}
	return r, nil
}

// backfillYTFields is one task of a backfill, in one transaction: the task's
// lane lock first (the lock prepareYTSnapshotTx takes, so a feed pass and the
// backfill decide one task one after the other), then the snapshot inputs,
// then the consumer's rule. A dry run applies the same rule
// (ytSnapshotUnchanged: an unchanged sha whose op was rejected counts as prepared)
// and rolls back.
func (s *Store) backfillYTFields(ctx context.Context, tenant string, id int64, dryRun bool, remaining int) (ytFieldsOutcome, int, string, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return "", 0, "", false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
		return "", 0, "", false, err
	}
	now := time.Now().UTC()
	tasks, err := readYTFieldsRows(ctx, tx, []int64{id}, now, s.StartedStalledHours())
	if err != nil {
		return "", 0, "", false, err
	}
	t, found := tasks[id]
	project := t.fields.Project
	// A dry run applies the consumer's rule as it would, and rolls back.
	outcome, ops, opID, err := prepareYTFieldsTx(ctx, tx, now, tenant, s.ytMovesNow() == YTMovesOn, id, t, found)
	if err != nil {
		return "", 0, "", false, err
	}
	if remaining > 0 && ops > remaining {
		// Roll back this task: its ops cannot fit within the run's remaining
		// budget. A later run, with more budget, starts from this task again.
		return "", 0, "", true, nil
	}
	if dryRun {
		return outcome, ops, project, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, "", false, err
	}
	logYTFieldsEnqueued(opID, id, t)
	return outcome, ops, project, false, nil
}

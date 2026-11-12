package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
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
// prepared in this run (0 for no cap); DryRun counts without writing.
type YTFieldsBackfillOptions struct {
	Project string
	TaskID  int64
	Limit   int
	DryRun  bool
}

// YTFieldsBackfillCounts is one class count per outcome of the consumer's
// rule. Scanned counts the tasks read; Prepared the ops made (or, in a dry
// run, that would be).
type YTFieldsBackfillCounts struct {
	Scanned     int `json:"scanned"`
	Prepared    int `json:"prepared"`
	Unchanged   int `json:"unchanged"`
	Unbound     int `json:"unbound"`
	Moved       int `json:"moved"`
	MoveSkipped int `json:"move_skipped"`
	OtherTenant int `json:"other_tenant"`
	Invalid     int `json:"invalid"`
}

func (c *YTFieldsBackfillCounts) add(o ytFieldsOutcome) {
	c.Scanned++
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
// in order of each project's first task id, and in total.
type YTFieldsBackfillReport struct {
	Tenant   string                     `json:"tenant"`
	Project  string                     `json:"project,omitempty"`
	TaskID   int64                      `json:"task_id,omitempty"`
	Limit    int                        `json:"limit,omitempty"`
	DryRun   bool                       `json:"dry_run"`
	Projects []*YTFieldsBackfillProject `json:"projects"`
	Total    YTFieldsBackfillCounts     `json:"total"`
}

// BackfillYTFieldsOps prepares the issue_fields snapshot of every task opt
// selects, in id order, each in its own transaction under the task's lane
// lock, by the consumer's rule and the consumer's gate (mode on and its
// tenant; off is ErrYTFieldsOff). It stops once opt.Limit ops are prepared;
// the next run starts again from the first task and passes the ones already
// sent as unchanged. A dry run reads the same rows and writes nothing.
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
	rows, err := s.pool.Query(ctx, `SELECT id FROM tasks WHERE ($1 = '' OR project = $1) AND ($2 = 0 OR id = $2) ORDER BY id`,
		opt.Project, opt.TaskID)
	if err != nil {
		return r, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return r, err
	}
	byProject := map[string]*YTFieldsBackfillProject{}
	for _, id := range ids {
		if opt.Limit > 0 && r.Total.Prepared >= opt.Limit {
			break
		}
		outcome, project, err := s.backfillYTFields(ctx, tenant, id, opt.DryRun)
		if err != nil {
			return r, fmt.Errorf("task %d: %w", id, err)
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
		p.add(outcome)
		r.Total.add(outcome)
	}
	return r, nil
}

// backfillYTFields is one task of a backfill, in one transaction: the task's
// lane lock first (the lock prepareYTSnapshotTx takes, so a feed pass and the
// backfill decide one task one after the other), then the snapshot inputs,
// then the consumer's rule. A dry run applies the same rule
// (ytSnapshotUnchanged: an unchanged sha whose op was rejected counts as prepared)
// and rolls back.
func (s *Store) backfillYTFields(ctx context.Context, tenant string, id int64, dryRun bool) (ytFieldsOutcome, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	tasks, err := readYTFieldsRows(ctx, tx, []int64{id}, now, s.StartedStalledHours())
	if err != nil {
		return "", "", err
	}
	t, found := tasks[id]
	project := t.fields.Project
	// A dry run applies the consumer's rule as it would, and rolls back.
	outcome, err := prepareYTFieldsTx(ctx, tx, now, tenant, id, t, found)
	if err != nil {
		return "", "", err
	}
	if dryRun {
		return outcome, project, nil
	}
	return outcome, project, tx.Commit(ctx)
}

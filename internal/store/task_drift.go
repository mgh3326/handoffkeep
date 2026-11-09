package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The drift report (hk 3766) names tasks whose state disagrees with the
// records around them. It only reports: nothing here moves a task, and the
// operator or the desk decides what to change.

// DriftDefaultMergedDays is the merged age, in days, past which a merged task
// with no deploy record is reported.
const DriftDefaultMergedDays = 2

// MaxDriftMergedDays bounds merged_days.
const MaxDriftMergedDays = 3650

// driftClassLimit bounds the tasks each class lists. A class with more
// candidates than this reports truncated.
const driftClassLimit = 200

// CheckDriftMergedDays reports whether days is a usable merged_days: 1..3650.
func CheckDriftMergedDays(days int) error {
	if days < 1 || days > MaxDriftMergedDays {
		return fmt.Errorf("merged_days must be 1..%d, got %d", MaxDriftMergedDays, days)
	}
	return nil
}

// TaskDrift is the read-only drift report at AsOf.
type TaskDrift struct {
	AsOf                time.Time      `json:"as_of"`
	MergedDays          int            `json:"merged_days"`
	StartedStalledHours int            `json:"started_stalled_hours"`
	SettledOpenDecision TaskDriftClass `json:"settled_open_decision"`
	MergedPRNotMerged   TaskDriftClass `json:"merged_pr_not_merged"`
	MergedWithoutDeploy TaskDriftClass `json:"merged_without_deploy"`
	IdleStarted         TaskDriftClass `json:"idle_started"`
}

// TaskDriftClass is one drift class: the rule it applies, the tasks it names,
// and whether it has more candidates than it lists (Truncated).
type TaskDriftClass struct {
	Rule      string          `json:"rule"`
	Count     int             `json:"count"`
	Truncated bool            `json:"truncated"`
	Tasks     []TaskDriftTask `json:"tasks"`
}

// TaskDriftTask is one task a class names. Since is the timestamp the class's
// rule measures from.
type TaskDriftTask struct {
	ID              int64     `json:"id"`
	Lane            string    `json:"lane"`
	Title           string    `json:"title"`
	Kind            string    `json:"kind"`
	State           string    `json:"state"`
	PR              string    `json:"pr,omitempty"`
	DecisionRequest string    `json:"decision_request,omitempty"`
	Since           time.Time `json:"since"`
}

// driftColumns is the TaskDriftTask row: id, lane, title, kind, state, pr,
// decision request id, and the class's Since.
const driftColumns = `id,lane,title,kind,state,COALESCE(refs->>'pr',''),COALESCE(refs->'decision_request'->>'id',''),`

// driftSettledOpenSQL is class (a): a settled task whose decision request is
// still open. The task-side status is what the request reads (decision_notify).
const driftSettledOpenSQL = `SELECT ` + driftColumns + `updated_at FROM tasks
	WHERE state IN (` + settledStatesSQL + `) AND refs->'decision_request'->>'status'=$1 ORDER BY id`

// driftMergedPRSQL is class (b): a non-settled task whose PR is merged. The PR
// is merged when the work record says so (a merged changes row for the URL) or
// a merged or deployed task already names it: the merge did not propagate to
// this task.
const driftMergedPRSQL = `SELECT ` + driftColumns + `updated_at FROM tasks t
	WHERE t.state NOT IN (` + settledStatesSQL + `) AND COALESCE(t.refs->>'pr','')<>'' AND (
		EXISTS (SELECT 1 FROM changes c WHERE c.state='merged' AND ('https://github.com/'||c.repo||'/pull/'||c.pr_number::text)=t.refs->>'pr')
		OR EXISTS (SELECT 1 FROM tasks m WHERE m.id<>t.id AND m.state IN ('merged','deployed') AND m.refs->>'pr'=t.refs->>'pr')
	) ORDER BY t.id`

// driftMergedPageSQL is one page of class (c)'s candidates: merged for at least
// the cutoff, after the (since, id) keyset cursor. Pages are read in order so
// the deploy-record filter runs before the class's cap, not after it.
const driftMergedPageSQL = `SELECT ` + driftColumns + `COALESCE(completed_at,updated_at) FROM tasks
	WHERE state='merged' AND COALESCE(completed_at,updated_at)<$1 AND (COALESCE(completed_at,updated_at),id)>($2,$3)
	ORDER BY COALESCE(completed_at,updated_at),id LIMIT $4`

// driftPageSize is how many candidates class (c) reads per page.
const driftPageSize = 200

// driftIdleStartedSQL is class (d): a researching or developing task with no
// activity since the cutoff (started_stalled_hours before AsOf).
const driftIdleStartedSQL = `SELECT ` + driftColumns + taskActivitySQL + ` FROM tasks
	WHERE state IN ('researching','developing') AND ` + taskActivitySQL + `<$1::timestamptz ORDER BY id`

// TaskDrift reads the four drift classes at now in one read-only snapshot, so
// the classes agree with each other. mergedDays is the merged age for class
// (c); class (d) uses the server's started_stalled_hours.
func (s *Store) TaskDrift(ctx context.Context, now time.Time, mergedDays int) (TaskDrift, error) {
	if err := CheckDriftMergedDays(mergedDays); err != nil {
		return TaskDrift{}, err
	}
	now = now.UTC()
	stalled := s.StartedStalledHours()
	out := TaskDrift{AsOf: now, MergedDays: mergedDays, StartedStalledHours: int(stalled.Hours())}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TaskDrift{}, err
	}
	defer tx.Rollback(ctx)

	if out.SettledOpenDecision, err = driftList(ctx, tx, "state is settled and refs.decision_request is open", driftSettledOpenSQL, DecisionRequestOpen); err != nil {
		return TaskDrift{}, fmt.Errorf("drift settled_open_decision: %w", err)
	}
	if out.MergedPRNotMerged, err = driftList(ctx, tx, "not settled, and refs.pr is merged: a merged changes row names the PR, or a merged or deployed task does", driftMergedPRSQL); err != nil {
		return TaskDrift{}, fmt.Errorf("drift merged_pr_not_merged: %w", err)
	}
	if out.MergedWithoutDeploy, err = driftMergedWithoutDeploy(ctx, tx, now.Add(-time.Duration(mergedDays)*24*time.Hour), mergedDays); err != nil {
		return TaskDrift{}, fmt.Errorf("drift merged_without_deploy: %w", err)
	}
	if out.IdleStarted, err = driftList(ctx, tx, fmt.Sprintf("researching or developing, with no activity for %d hours (started_stalled_hours)", out.StartedStalledHours), driftIdleStartedSQL, now.Add(-stalled)); err != nil {
		return TaskDrift{}, fmt.Errorf("drift idle_started: %w", err)
	}
	return out, tx.Commit(ctx)
}

// driftMergedWithoutDeploy is class (c): merged candidates older than the
// cutoff that no deploy record lists. A task with no PR cannot be listed by a
// record, so it is reported.
func driftMergedWithoutDeploy(ctx context.Context, tx pgx.Tx, cutoff time.Time, mergedDays int) (TaskDriftClass, error) {
	out := TaskDriftClass{
		Rule:  fmt.Sprintf("merged for at least %d days, and no deploy record lists refs.pr", mergedDays),
		Tasks: []TaskDriftTask{},
	}
	// Candidates are paged by keyset. A page is filtered by its deploy records
	// before the class cap applies, so the cap counts qualifying tasks only:
	// Truncated means a qualifying task beyond the listed ones exists.
	after, afterID := time.Unix(0, 0).UTC(), int64(0)
scan:
	for {
		rows, err := tx.Query(ctx, driftMergedPageSQL, cutoff, after, afterID, driftPageSize)
		if err != nil {
			return TaskDriftClass{}, err
		}
		page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaskDriftTask, error) {
			var x TaskDriftTask
			err := row.Scan(&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.PR, &x.DecisionRequest, &x.Since)
			return x, err
		})
		if err != nil {
			return TaskDriftClass{}, err
		}
		for _, x := range page {
			if x.PR != "" {
				key, err := deployRecordKeyForPRTx(ctx, tx, x.PR)
				if err != nil {
					return TaskDriftClass{}, err
				}
				if key != "" {
					continue
				}
			}
			if len(out.Tasks) == driftClassLimit {
				out.Truncated = true
				break scan
			}
			out.Tasks = append(out.Tasks, x)
		}
		if len(page) < driftPageSize {
			break
		}
		last := page[len(page)-1]
		after, afterID = last.Since, last.ID
	}
	out.Count = len(out.Tasks)
	return out, nil
}

// driftList runs one class query, which takes its args as $1.. and ends in an
// ORDER BY on a unique key. It lists at most driftClassLimit tasks and reports
// Truncated when there are more.
func driftList(ctx context.Context, tx pgx.Tx, rule, sql string, args ...any) (TaskDriftClass, error) {
	args = append(args, driftClassLimit+1)
	rows, err := tx.Query(ctx, sql+fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return TaskDriftClass{}, err
	}
	xs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaskDriftTask, error) {
		var x TaskDriftTask
		err := row.Scan(&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.PR, &x.DecisionRequest, &x.Since)
		return x, err
	})
	if err != nil {
		return TaskDriftClass{}, err
	}
	c := TaskDriftClass{Rule: rule, Tasks: []TaskDriftTask{}}
	if len(xs) > driftClassLimit {
		xs, c.Truncated = xs[:driftClassLimit], true
	}
	c.Tasks, c.Count = xs, len(xs)
	return c, nil
}

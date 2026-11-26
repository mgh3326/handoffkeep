package store

import (
	"context"
	"fmt"
	"strings"
	"time"
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
	AutoCloseHeld       TaskDriftClass `json:"auto_close_held"`
	MergedWithoutDeploy TaskDriftClass `json:"merged_without_deploy"`
	IdleStarted         TaskDriftClass `json:"idle_started"`
	// ExplicitAllSettled is hk 5112's class: an explicit task whose pieces are
	// all settled but which is still open. Hygiene never applies it.
	ExplicitAllSettled TaskDriftClass `json:"explicit_all_settled"`
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

	// What holds the task's close (taskHoldsTx and closeGateTx), and whether
	// the hygiene job may apply it now (driftMechanical).
	OpenPRs      []string `json:"open_prs,omitempty"`
	RevertedPRs  []string `json:"reverted_prs,omitempty"`
	OpenSubTasks []int64  `json:"open_sub_tasks,omitempty"`
	Hold         string   `json:"hold,omitempty"`
	Mechanical   bool     `json:"mechanical"`
}

// driftColumns is the TaskDriftTask row: id, lane, title, kind, state, pr,
// decision request id, and the class's Since.
const driftColumns = `id,lane,title,kind,state,COALESCE(refs->>'pr',''),COALESCE(refs->'decision_request'->>'id',''),`

// driftColumnsMySQL is driftColumns for MySQL.
var driftColumnsMySQL = `id,lane,title,kind,state,COALESCE(` + reportJSONText("refs", "$.pr") + `,''),COALESCE(` + reportJSONText("refs", "$.decision_request.id") + `,''),`

// driftSettledOpenSQL is class (a): a settled task whose decision request is
// still open. The task-side status is what the request reads (decision_notify).
const driftSettledOpenSQL = `SELECT ` + driftColumns + `updated_at FROM tasks
	WHERE state IN (` + settledStatesSQL + `) AND refs->'decision_request'->>'status'=$1 ORDER BY id`

var driftSettledOpenSQLMySQL = `SELECT ` + driftColumnsMySQL + `updated_at FROM tasks
	WHERE state IN (` + settledStatesSQL + `) AND ` + reportJSONText("refs", "$.decision_request.status") + `=? ORDER BY id`

// driftMergedPRSQL is class (b): a non-settled task whose PR is merged, and no
// unrelanded revert holds it (unresolvedRevertSQL). The PR is merged when one
// of the task's implements changes is merged and not reverted (change_tasks;
// the task names no PR, as a held merge leaves it), or when the work record
// says so for the PR the task names (a merged changes row, or a merged or
// deployed task that names it): the merge did not propagate to this task.
var driftMergedPRSQL = `SELECT ` + driftColumns + `updated_at FROM tasks t
	WHERE t.state NOT IN (` + settledStatesSQL + `) AND t.close_rule<>'explicit' AND (
		EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
			WHERE ct.task_id=t.id AND ct.relation='implements' AND c.state='merged' AND c.reverted_by_change_id IS NULL)
		OR (COALESCE(t.refs->>'pr','')<>'' AND (
			EXISTS (SELECT 1 FROM changes c WHERE c.state='merged' AND (` + prURLSQL + `)=t.refs->>'pr')
			OR EXISTS (SELECT 1 FROM tasks m WHERE m.id<>t.id AND m.state IN ('merged','deployed') AND m.refs->>'pr'=t.refs->>'pr')))
	) AND NOT EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ` + unresolvedRevertSQL("t.id") + `)
	ORDER BY t.id`

// driftMergedPRSQLMySQL is driftMergedPRSQL for MySQL.
var driftMergedPRSQLMySQL = `SELECT ` + driftColumnsMySQL + `updated_at FROM tasks t
	WHERE t.state NOT IN (` + settledStatesSQL + `) AND t.close_rule<>'explicit' AND (
		EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
			WHERE ct.task_id=t.id AND ct.relation='implements' AND c.state='merged' AND c.reverted_by_change_id IS NULL)
		OR (COALESCE(` + reportJSONText("t.refs", "$.pr") + `,'')<>'' AND (
			EXISTS (SELECT 1 FROM changes c WHERE c.state='merged' AND (` + reportPRURLSQL + `)=` + reportJSONText("t.refs", "$.pr") + `)
			OR EXISTS (SELECT 1 FROM tasks m WHERE m.id<>t.id AND m.state IN ('merged','deployed') AND ` + reportJSONText("m.refs", "$.pr") + `=` + reportJSONText("t.refs", "$.pr") + `)))
	) AND NOT EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ` + unresolvedRevertMySQL("t.id") + `)
	ORDER BY t.id`

// driftAutoCloseHeldSQL is class (e): an unsettled parent in an auto-close
// state whose sub-tasks are all settled, one of which did work, and whose
// automatic close would move it to merged or deployed (or to completed, for a
// decide, ops, verify or research parent, the kinds canCompleteKind names). The close is
// due and was not made: it is held, or was held and nothing re-ran it. An
// explicit parent is not in the class: its close is by hand (driftExplicitSettledSQL).
var driftAutoCloseHeldSQL = `SELECT ` + driftColumns + `updated_at FROM tasks p
	WHERE p.state IN (` + autoCloseStatesSQL + `) AND p.close_rule<>'explicit'
	AND EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state IN ('completed','merged','deployed'))
	AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state NOT IN (` + settledStatesSQL + `))
	AND (p.kind IN ('decide','ops','verify','research') OR EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state IN ('merged','deployed')))
	ORDER BY p.id`

// driftAutoCloseHeldSQLMySQL is driftAutoCloseHeldSQL for MySQL: only the
// selected columns differ.
var driftAutoCloseHeldSQLMySQL = strings.Replace(driftAutoCloseHeldSQL, driftColumns, driftColumnsMySQL, 1)

// driftExplicitSettledSQL is class (f), explicit_all_settled: an explicit task
// in an auto-close state (the states an automatic close applies from, so
// triage and icebox are out, as no close is due there) that is still open
// although every change it implements has merged (and is not reverted) and
// every sub-task is settled. It needs at least one piece, a change or a
// sub-task, so an empty explicit task is not listed. Each row is a reminder for
// the task's owner, who closes it by hand; nothing applies it.
var driftExplicitSettledSQL = `SELECT ` + driftColumns + `updated_at FROM tasks t
	WHERE t.close_rule='explicit' AND t.state IN (` + autoCloseStatesSQL + `) AND (
		EXISTS (SELECT 1 FROM change_tasks ct WHERE ct.task_id=t.id AND ct.relation='implements')
		OR EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=t.id)
	) AND NOT EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
		WHERE ct.task_id=t.id AND ct.relation='implements' AND NOT (c.state='merged' AND c.reverted_by_change_id IS NULL))
	AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=t.id AND k.state NOT IN (` + settledStatesSQL + `))
	ORDER BY t.id`

// driftExplicitSettledSQLMySQL is driftExplicitSettledSQL for MySQL: only the
// selected columns differ.
var driftExplicitSettledSQLMySQL = strings.Replace(driftExplicitSettledSQL, driftColumns, driftColumnsMySQL, 1)

// driftMergedPageSQL is one page of class (c)'s candidates: merged for at least
// the cutoff, after the (since, id) keyset cursor. Pages are read in order so
// the deploy-record filter runs before the class's cap, not after it.
const driftMergedPageSQL = `SELECT ` + driftColumns + `COALESCE(completed_at,updated_at) FROM tasks
	WHERE state='merged' AND COALESCE(completed_at,updated_at)<$1 AND (COALESCE(completed_at,updated_at),id)>($2,$3)
	ORDER BY COALESCE(completed_at,updated_at),id LIMIT $4`

// driftMergedPageSQLMySQL is driftMergedPageSQL for MySQL.
var driftMergedPageSQLMySQL = `SELECT ` + driftColumnsMySQL + `COALESCE(completed_at,updated_at) FROM tasks
	WHERE state='merged' AND COALESCE(completed_at,updated_at)<? AND (COALESCE(completed_at,updated_at),id)>(?,?)
	ORDER BY COALESCE(completed_at,updated_at),id LIMIT ?`

// driftPageSize is how many candidates class (c) reads per page.
const driftPageSize = 200

// driftIdleStartedSQL is class (d): a researching or developing task with no
// activity since the cutoff (started_stalled_hours before AsOf).
const driftIdleStartedSQL = `SELECT ` + driftColumns + taskActivitySQL + ` FROM tasks
	WHERE state IN ('researching','developing') AND ` + taskActivitySQL + `<$1::timestamptz ORDER BY id`

var driftIdleStartedSQLMySQL = `SELECT ` + driftColumnsMySQL + taskActivitySQLMySQL + ` FROM tasks
	WHERE state IN ('researching','developing') AND ` + taskActivitySQLMySQL + `<? ORDER BY id`

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
	tx, err := s.beginReportRead(ctx, true)
	if err != nil {
		return TaskDrift{}, err
	}
	defer tx.Rollback(ctx)

	if out.SettledOpenDecision, err = driftList(ctx, tx, "state is settled and refs.decision_request is open", sqlFor(tx, driftSettledOpenSQL, driftSettledOpenSQLMySQL), DecisionRequestOpen); err != nil {
		return TaskDrift{}, fmt.Errorf("drift settled_open_decision: %w", err)
	}
	if out.MergedPRNotMerged, err = driftList(ctx, tx, "not settled, with a merged PR: an implements change is merged and not reverted, or refs.pr is merged (a merged changes row names it, or a merged or deployed task does); no unrelanded revert", sqlFor(tx, driftMergedPRSQL, driftMergedPRSQLMySQL)); err != nil {
		return TaskDrift{}, fmt.Errorf("drift merged_pr_not_merged: %w", err)
	}
	if err = s.annotateDriftHolds(ctx, tx, &out.MergedPRNotMerged, mergeAppliesFrom); err != nil {
		return TaskDrift{}, fmt.Errorf("drift merged_pr_not_merged: %w", err)
	}
	if out.AutoCloseHeld, err = driftList(ctx, tx, "auto-close due and not made: an unsettled parent in an auto-close state, every sub-task settled and one did work, moving it to merged, deployed, or completed for decide, ops, verify and research", sqlFor(tx, driftAutoCloseHeldSQL, driftAutoCloseHeldSQLMySQL)); err != nil {
		return TaskDrift{}, fmt.Errorf("drift auto_close_held: %w", err)
	}
	if err = s.annotateDriftHolds(ctx, tx, &out.AutoCloseHeld, autoCloseAppliesFrom); err != nil {
		return TaskDrift{}, fmt.Errorf("drift auto_close_held: %w", err)
	}
	if out.MergedWithoutDeploy, err = driftMergedWithoutDeploy(ctx, tx, now.Add(-time.Duration(mergedDays)*24*time.Hour), mergedDays); err != nil {
		return TaskDrift{}, fmt.Errorf("drift merged_without_deploy: %w", err)
	}
	if out.IdleStarted, err = driftList(ctx, tx, fmt.Sprintf("researching or developing, with no activity for %d hours (started_stalled_hours)", out.StartedStalledHours), sqlFor(tx, driftIdleStartedSQL, driftIdleStartedSQLMySQL), now.Add(-stalled)); err != nil {
		return TaskDrift{}, fmt.Errorf("drift idle_started: %w", err)
	}
	if out.ExplicitAllSettled, err = driftList(ctx, tx, "explicit (close_rule): every implements change merged and not reverted, every sub-task settled, and the task still open; a person closes it", sqlFor(tx, driftExplicitSettledSQL, driftExplicitSettledSQLMySQL)); err != nil {
		return TaskDrift{}, fmt.Errorf("drift explicit_all_settled: %w", err)
	}
	return out, tx.Commit(ctx)
}

// driftMergedWithoutDeploy is class (c): merged candidates older than the
// cutoff that no deploy record lists. A task with no PR cannot be listed by a
// record, so it is reported.
func driftMergedWithoutDeploy(ctx context.Context, tx transaction, cutoff time.Time, mergedDays int) (TaskDriftClass, error) {
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
		rows, err := tx.Query(ctx, sqlFor(tx, driftMergedPageSQL, driftMergedPageSQLMySQL), cutoff, after, afterID, driftPageSize)
		if err != nil {
			return TaskDriftClass{}, err
		}
		page, err := collectRows(rows, func(row row) (TaskDriftTask, error) {
			var x TaskDriftTask
			err := row.Scan(&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.PR, &x.DecisionRequest, &x.Since)
			return x, err
		})
		if err != nil {
			return TaskDriftClass{}, err
		}
		// One read answers every PR of the page (hk 5058): a per-row lookup
		// ran one deploy-record scan per PR, 114 per request in production.
		listed, err := deployRecordedPRsTx(ctx, tx, pagePRs(page))
		if err != nil {
			return TaskDriftClass{}, err
		}
		for _, x := range page {
			if x.PR != "" && listed[x.PR] {
				continue
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

// pagePRs lists the distinct PRs of a page, in page order, for one batched
// deploy-record read.
func pagePRs(page []TaskDriftTask) []string {
	seen := map[string]bool{}
	var prs []string
	for _, x := range page {
		if x.PR != "" && !seen[x.PR] {
			seen[x.PR] = true
			prs = append(prs, x.PR)
		}
	}
	return prs
}

// mergeAppliesFrom reports whether a merge event moves a task in state (the
// merge row of sourceEventTable).
func mergeAppliesFrom(state string) bool {
	return sourceEventTable[SourceMergewakeMerge].from[state]
}

// driftMechanical reports whether the hygiene job (hk 3292) may apply a task's
// close now: nothing holds it, its state is one applies accepts, and
// closeGateTx passes (gate is its reason, "" when it passes).
func driftMechanical(h taskHolds, gate, state string, applies func(state string) bool) bool {
	return h.none() && gate == "" && applies(state)
}

// annotateDriftHolds fills, for each task a class names, what holds its close:
// open_prs, reverted_prs, open_sub_tasks, and hold (the first reason). It sets
// mechanical through driftMechanical. It only reads.
func (s *Store) annotateDriftHolds(ctx context.Context, tx transaction, c *TaskDriftClass, applies func(state string) bool) error {
	for i := range c.Tasks {
		x := &c.Tasks[i]
		var t Task
		if err := scanTask(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, `SELECT `+taskColumns+` FROM tasks WHERE id=?`), x.ID), &t); err != nil {
			return err
		}
		pr := x.PR
		if verified, err := hygieneVerifiedPRTx(ctx, tx, t); err != nil {
			return err
		} else if verified != "" {
			pr = verified
		}
		if pr == "" {
			heldPR, err := heldMergePRTx(ctx, tx, x.ID)
			if err != nil {
				return err
			}
			pr = heldPR
		}
		h, err := taskHoldsTx(ctx, tx, x.ID, pr)
		if err != nil {
			return err
		}
		gate, err := s.closeGateTx(ctx, tx, t, pr)
		if err != nil {
			return err
		}
		x.OpenPRs, x.RevertedPRs, x.OpenSubTasks = h.OpenPRs, h.RevertedPRs, h.OpenSubTasks
		x.Hold = h.mergeReason()
		if x.Hold == "" {
			x.Hold = gate
		}
		x.Mechanical = driftMechanical(h, gate, t.State, applies)
	}
	return nil
}

// driftList runs one class query, which takes its args as $1.. and ends in an
// ORDER BY on a unique key. It lists at most driftClassLimit tasks and reports
// Truncated when there are more.
func driftList(ctx context.Context, tx transaction, rule, sql string, args ...any) (TaskDriftClass, error) {
	args = append(args, driftClassLimit+1)
	limit := fmt.Sprintf(" LIMIT $%d", len(args))
	if dialectFor(tx) == mysqlDialect {
		limit = " LIMIT ?"
	}
	rows, err := tx.Query(ctx, sql+limit, args...)
	if err != nil {
		return TaskDriftClass{}, err
	}
	xs, err := collectRows(rows, func(row row) (TaskDriftTask, error) {
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

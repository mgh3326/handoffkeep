package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskHygieneReport is the result of the daily hygiene sweep (hk 3292). It
// reports mechanical closes (applied when dryRun=false, or prospective when
// dryRun=true) and findings that require operator attention.
type TaskHygieneReport struct {
	AsOf             time.Time           `json:"as_of"`
	DryRun           bool                `json:"dry_run"`
	Project          string              `json:"project,omitempty"`
	MechanicalCloses []TaskHygieneClose  `json:"mechanical_closes"`
	Findings         TaskHygieneFindings `json:"findings"`
	SettleMerged     *SettleMergedReport `json:"settle_merged,omitempty"`
}

// TaskHygieneClose records one task closed mechanically.
type TaskHygieneClose struct {
	TaskID    int64  `json:"task_id"`
	Title     string `json:"title"`
	Lane      string `json:"lane"`
	Kind      string `json:"kind"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Rule      string `json:"rule"`
	PR        string `json:"pr,omitempty"`
}

// TaskHygieneFindings gathers discrepancy rows that could not be closed
// mechanically and therefore require operator triage.
type TaskHygieneFindings struct {
	MergedPROpenTasks             []TaskDriftTask              `json:"merged_pr_open_tasks"`
	AutoCloseHeldParents          []TaskDriftTask              `json:"auto_close_held_parents"`
	SettledOpenDecisions          []TaskDriftTask              `json:"settled_open_decisions"`
	LiveAttemptsWithoutLease      []TaskDriftTask              `json:"live_attempts_without_lease"`
	AnsweredDecisionsNotDelivered []TaskHygieneLaggingDecision `json:"answered_decisions_not_delivered"`
	MergedWithoutDeploy           []TaskDriftTask              `json:"merged_without_deploy,omitempty"`
}

// TaskHygieneLaggingDecision is one decision delivery lagging behind its
// request's effective status for longer than DecisionDeliveryLagAge past its
// next attempt.
type TaskHygieneLaggingDecision struct {
	RequestID      int64     `json:"request_id"`
	Destination    string    `json:"destination"`
	TaskTitle      string    `json:"task_title,omitempty"`
	DesiredVersion string    `json:"desired_version"`
	AppliedVersion string    `json:"applied_version"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	LastErrorCode  string    `json:"last_error_code,omitempty"`
}

// hygieneLaggingDecisionsSQL selects decision_deliveries rows lagging behind
// effective status past DecisionDeliveryLagAge. $1 is now (for effective status),
// $2 is the lag cutoff, and $3 is the optional project filter.
const hygieneLaggingDecisionsSQL = `SELECT n.request_id, n.destination, COALESCE(t.title, ''), n.desired_version, n.applied_version, n.attempts, n.next_attempt_at, n.last_error_code
FROM decision_deliveries n
JOIN decision_requests d ON d.id = n.request_id
LEFT JOIN tasks t ON t.id = d.task_id
WHERE n.state = 'pending'
  AND n.applied_version <> ` + decisionEffectiveStatusSQL + `
  AND n.next_attempt_at < $2::timestamptz
  AND ($3::text IS NULL OR t.project = $3)
ORDER BY n.request_id`

// TaskHygiene runs the daily hygiene job (hk 3292): it checks for merged PRs
// with open tasks, live attempts without lease (idle started tasks), and
// answered decisions not delivered. Mechanical closes are applied when dryRun
// is false, and findings are gathered for operator inbox attention.
func (s *Store) TaskHygiene(ctx context.Context, now time.Time, project string, dryRun bool) (TaskHygieneReport, error) {
	if project != "" && !validName(project) {
		return TaskHygieneReport{}, errors.New("invalid task hygiene: project must be a project name")
	}
	now = now.UTC()
	rep := TaskHygieneReport{
		AsOf:             now,
		DryRun:           dryRun,
		Project:          project,
		MechanicalCloses: []TaskHygieneClose{},
		Findings: TaskHygieneFindings{
			MergedPROpenTasks:             []TaskDriftTask{},
			AutoCloseHeldParents:          []TaskDriftTask{},
			SettledOpenDecisions:          []TaskDriftTask{},
			LiveAttemptsWithoutLease:      []TaskDriftTask{},
			AnsweredDecisionsNotDelivered: []TaskHygieneLaggingDecision{},
			MergedWithoutDeploy:           []TaskDriftTask{},
		},
	}

	drift, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
	if err != nil {
		return TaskHygieneReport{}, fmt.Errorf("task hygiene drift: %w", err)
	}

	// Filter drift classes by project if specified.
	if project != "" {
		filterDriftProject(ctx, s.pool, &drift, project)
	}

	// Gather lagging decisions.
	lagCutoff := now.Add(-DecisionDeliveryLagAge)
	var projArg *string
	if project != "" {
		projArg = &project
	}
	rows, err := s.pool.Query(ctx, hygieneLaggingDecisionsSQL, now, lagCutoff, projArg)
	if err != nil {
		return TaskHygieneReport{}, fmt.Errorf("task hygiene lagging decisions: %w", err)
	}
	for rows.Next() {
		var d TaskHygieneLaggingDecision
		if err := rows.Scan(&d.RequestID, &d.Destination, &d.TaskTitle, &d.DesiredVersion, &d.AppliedVersion, &d.Attempts, &d.NextAttemptAt, &d.LastErrorCode); err != nil {
			rows.Close()
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan lagging decision: %w", err)
		}
		d.NextAttemptAt = d.NextAttemptAt.UTC()
		rep.Findings.AnsweredDecisionsNotDelivered = append(rep.Findings.AnsweredDecisionsNotDelivered, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TaskHygieneReport{}, fmt.Errorf("task hygiene lagging decisions rows: %w", err)
	}

	// Categorize drift candidates into mechanical vs findings.
	var mechanicalMergedTasks []TaskDriftTask
	for _, x := range drift.MergedPRNotMerged.Tasks {
		if x.Mechanical {
			mechanicalMergedTasks = append(mechanicalMergedTasks, x)
		} else {
			rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, x)
		}
	}

	var mechanicalAutoCloseParents []TaskDriftTask
	for _, x := range drift.AutoCloseHeld.Tasks {
		if x.Mechanical {
			mechanicalAutoCloseParents = append(mechanicalAutoCloseParents, x)
		} else {
			rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, x)
		}
	}

	rep.Findings.SettledOpenDecisions = append(rep.Findings.SettledOpenDecisions, drift.SettledOpenDecision.Tasks...)
	rep.Findings.LiveAttemptsWithoutLease = append(rep.Findings.LiveAttemptsWithoutLease, drift.IdleStarted.Tasks...)
	rep.Findings.MergedWithoutDeploy = append(rep.Findings.MergedWithoutDeploy, drift.MergedWithoutDeploy.Tasks...)

	if dryRun {
		// In dry-run mode, report prospective mechanical closes without writing.
		for _, x := range mechanicalMergedTasks {
			toState := StateMerged
			if canCompleteKind(x.Kind) {
				toState = StateCompleted
			}
			rep.MechanicalCloses = append(rep.MechanicalCloses, TaskHygieneClose{
				TaskID:    x.ID,
				Title:     x.Title,
				Lane:      x.Lane,
				Kind:      x.Kind,
				FromState: x.State,
				ToState:   toState,
				Rule:      "merged_pr",
				PR:        x.PR,
			})
		}
		for _, x := range mechanicalAutoCloseParents {
			roll, err := s.subTaskRollupForParent(ctx, x.ID)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			rep.MechanicalCloses = append(rep.MechanicalCloses, TaskHygieneClose{
				TaskID:    x.ID,
				Title:     x.Title,
				Lane:      x.Lane,
				Kind:      x.Kind,
				FromState: x.State,
				ToState:   roll.closeTarget(),
				Rule:      "auto_close_parent",
			})
		}
	} else {
		// In apply mode, execute the mechanical closes under the level-2 task mutation lock.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return TaskHygieneReport{}, err
		}
		defer tx.Rollback(ctx)

		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
			return TaskHygieneReport{}, err
		}

		for _, cand := range mechanicalMergedTasks {
			var t Task
			if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, cand.ID), &t); err != nil {
				return TaskHygieneReport{}, err
			}
			if project != "" && (t.Project == nil || *t.Project != project) {
				continue
			}
			h, err := taskHoldsTx(ctx, tx, t.ID, "")
			if err != nil {
				return TaskHygieneReport{}, err
			}
			gate, err := s.closeGateTx(ctx, tx, t)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			if !driftMechanical(h, gate, t.State, mergeAppliesFrom) {
				rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, cand)
				continue
			}
			pr := t.Refs.PR
			if pr == "" {
				pr, _ = heldMergePRTx(ctx, tx, t.ID)
			}
			if pr == "" {
				_ = tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(t.refs->>'pr',''), c.pr_url, `+prURLSQL+`) FROM tasks t JOIN change_tasks ct ON ct.task_id=t.id AND ct.relation='implements' JOIN changes c ON c.id=ct.change_id AND c.state='merged' AND c.reverted_by_change_id IS NULL WHERE t.id=$1 ORDER BY c.id DESC LIMIT 1`, t.ID).Scan(&pr)
			}
			if pr == "" {
				rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, cand)
				continue
			}
			fromState := t.State
			ev := sourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, PR: pr, At: now, Note: "hygiene: mechanical close of merged PR"}
			res, err := s.applySourceEventTx(ctx, tx, t, ev, now)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			if res.Applied {
				rep.MechanicalCloses = append(rep.MechanicalCloses, TaskHygieneClose{
					TaskID:    t.ID,
					Title:     t.Title,
					Lane:      t.Lane,
					Kind:      t.Kind,
					FromState: fromState,
					ToState:   res.State,
					Rule:      "merged_pr",
					PR:        pr,
				})
			}
		}

		for _, cand := range mechanicalAutoCloseParents {
			var p Task
			if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, cand.ID), &p); err != nil {
				return TaskHygieneReport{}, err
			}
			if project != "" && (p.Project == nil || *p.Project != project) {
				continue
			}
			h, err := taskHoldsTx(ctx, tx, p.ID, "")
			if err != nil {
				return TaskHygieneReport{}, err
			}
			gate, err := s.closeGateTx(ctx, tx, p)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			if !driftMechanical(h, gate, p.State, autoCloseAppliesFrom) {
				rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, cand)
				continue
			}
			var childID int64
			var childState string
			err = tx.QueryRow(ctx, `SELECT id, state FROM tasks WHERE parent_id=$1 AND state IN ('completed','merged','deployed') ORDER BY updated_at DESC LIMIT 1`, p.ID).Scan(&childID, &childState)
			if err != nil {
				rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, cand)
				continue
			}
			fromState := p.State
			closed, nextP, err := s.autoCloseOneParentTx(ctx, tx, p, childID, childState, now)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			if closed {
				rep.MechanicalCloses = append(rep.MechanicalCloses, TaskHygieneClose{
					TaskID:    p.ID,
					Title:     p.Title,
					Lane:      p.Lane,
					Kind:      p.Kind,
					FromState: fromState,
					ToState:   nextP.State,
					Rule:      "auto_close_parent",
				})
				if nextP.ParentID != nil {
					if err := s.autoCloseTx(ctx, tx, nextP, "hk", now); err != nil {
						return TaskHygieneReport{}, err
					}
				}
			}
		}

		if err := tx.Commit(ctx); err != nil {
			return TaskHygieneReport{}, err
		}
	}

	settleRep, err := s.SettleMerged(ctx, project, dryRun)
	if err != nil {
		return TaskHygieneReport{}, fmt.Errorf("task hygiene settle-merged: %w", err)
	}
	rep.SettleMerged = &settleRep

	return rep, nil
}

func (s *Store) subTaskRollupForParent(ctx context.Context, parentID int64) (subTaskRollup, error) {
	rows, err := s.pool.Query(ctx, `SELECT state FROM tasks WHERE parent_id=$1`, parentID)
	if err != nil {
		return subTaskRollup{}, err
	}
	sub, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return subTaskRollup{}, err
	}
	return rollupStates(sub), nil
}

func filterDriftProject(ctx context.Context, q poolQuerier, d *TaskDrift, project string) {
	ids := map[int64]bool{}
	collect := func(c TaskDriftClass) {
		for _, x := range c.Tasks {
			ids[x.ID] = true
		}
	}
	collect(d.SettledOpenDecision)
	collect(d.MergedPRNotMerged)
	collect(d.AutoCloseHeld)
	collect(d.MergedWithoutDeploy)
	collect(d.IdleStarted)
	if len(ids) == 0 {
		return
	}
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	rows, err := q.Query(ctx, `SELECT id, project FROM tasks WHERE id=ANY($1)`, idList)
	if err != nil {
		return
	}
	defer rows.Close()
	projMap := map[int64]string{}
	for rows.Next() {
		var id int64
		var p *string
		if err := rows.Scan(&id, &p); err == nil && p != nil {
			projMap[id] = *p
		}
	}
	filterClass := func(c *TaskDriftClass) {
		filtered := []TaskDriftTask{}
		for _, x := range c.Tasks {
			if projMap[x.ID] == project {
				filtered = append(filtered, x)
			}
		}
		c.Tasks = filtered
		c.Count = len(filtered)
	}
	filterClass(&d.SettledOpenDecision)
	filterClass(&d.MergedPRNotMerged)
	filterClass(&d.AutoCloseHeld)
	filterClass(&d.MergedWithoutDeploy)
	filterClass(&d.IdleStarted)
}

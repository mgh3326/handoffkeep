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
	MergedPROpenTasks                      []TaskDriftTask              `json:"merged_pr_open_tasks"`
	MergedPROpenTasksTruncated             bool                         `json:"merged_pr_open_tasks_truncated,omitempty"`
	AutoCloseHeldParents                   []TaskDriftTask              `json:"auto_close_held_parents"`
	AutoCloseHeldParentsTruncated          bool                         `json:"auto_close_held_parents_truncated,omitempty"`
	SettledOpenDecisions                   []TaskDriftTask              `json:"settled_open_decisions"`
	SettledOpenDecisionsTruncated          bool                         `json:"settled_open_decisions_truncated,omitempty"`
	LiveAttemptsWithoutLease               []TaskDriftTask              `json:"live_attempts_without_lease"`
	LiveAttemptsWithoutLeaseTruncated      bool                         `json:"live_attempts_without_lease_truncated,omitempty"`
	AnsweredDecisionsNotDelivered          []TaskHygieneLaggingDecision `json:"answered_decisions_not_delivered"`
	AnsweredDecisionsNotDeliveredTruncated bool                         `json:"answered_decisions_not_delivered_truncated,omitempty"`
	MergedWithoutDeploy                    []TaskDriftTask              `json:"merged_without_deploy,omitempty"`
	MergedWithoutDeployTruncated           bool                         `json:"merged_without_deploy_truncated,omitempty"`
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

const (
	hygieneFindingsLimit = 200
	hygienePageSize      = 200
)

// hygieneLaggingDecisionsSQL selects decision_deliveries rows lagging behind
// effective status past DecisionDeliveryLagAge. $1 is now (for effective status),
// $2 is the lag cutoff, $3 is the optional project filter, $4 is the request_id cursor,
// and $5 is the limit.
const hygieneLaggingDecisionsSQL = `SELECT n.request_id, n.destination, COALESCE(t.title, ''), n.desired_version, n.applied_version, n.attempts, n.next_attempt_at, n.last_error_code
FROM decision_deliveries n
JOIN decision_requests d ON d.id = n.request_id
LEFT JOIN tasks t ON t.id = d.task_id
WHERE n.state = 'pending'
  AND n.applied_version <> ` + decisionEffectiveStatusSQL + `
  AND n.next_attempt_at < $2::timestamptz
  AND ($3::text IS NULL OR t.project = $3)
  AND n.request_id > $4
ORDER BY n.request_id LIMIT $5`

// hygieneMergedPRPageSQL pages candidates for Mergewake PR not merged.
// $1 is id cursor (t.id > $1), $2 is project (or NULL), $3 is limit.
var hygieneMergedPRPageSQL = `SELECT ` + driftColumns + `t.updated_at FROM tasks t
	WHERE t.state NOT IN (` + settledStatesSQL + `)
	  AND t.id > $1
	  AND ($2::text IS NULL OR t.project = $2)
	  AND (
		EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
			WHERE ct.task_id=t.id AND ct.relation='implements' AND c.state='merged' AND c.reverted_by_change_id IS NULL)
		OR (COALESCE(t.refs->>'pr','')<>'' AND (
			EXISTS (SELECT 1 FROM changes c WHERE c.state='merged' AND (` + prURLSQL + `)=t.refs->>'pr')
			OR EXISTS (SELECT 1 FROM tasks m WHERE m.id<>t.id AND m.state IN ('merged','deployed') AND m.refs->>'pr'=t.refs->>'pr')))
	) AND NOT EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ` + unresolvedRevertSQL("t.id") + `)
	ORDER BY t.id LIMIT $3`

// hygieneAutoCloseHeldPageSQL pages candidates for auto-close held parents.
// $1 is id cursor (p.id > $1), $2 is project (or NULL), $3 is limit.
var hygieneAutoCloseHeldPageSQL = `SELECT ` + driftColumns + `p.updated_at FROM tasks p
	WHERE p.state IN (` + autoCloseStatesSQL + `)
	  AND p.id > $1
	  AND ($2::text IS NULL OR p.project = $2)
	  AND EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state IN ('completed','merged','deployed'))
	  AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state NOT IN (` + settledStatesSQL + `))
	  AND (p.kind IN ('decide','ops','verify','research') OR EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=p.id AND k.state IN ('merged','deployed')))
	ORDER BY p.id LIMIT $3`

// hygieneSettledOpenPageSQL pages settled open decisions.
// $1 is status ('open'), $2 is id cursor (t.id > $2), $3 is project (or NULL), $4 is limit.
const hygieneSettledOpenPageSQL = `SELECT ` + driftColumns + `updated_at FROM tasks t
	WHERE t.state IN (` + settledStatesSQL + `)
	  AND t.refs->'decision_request'->>'status'=$1
	  AND t.id > $2
	  AND ($3::text IS NULL OR t.project = $3)
	ORDER BY t.id LIMIT $4`

// hygieneIdleStartedPageSQL pages idle started tasks.
// $1 is cutoff time, $2 is id cursor (t.id > $2), $3 is project (or NULL), $4 is limit.
const hygieneIdleStartedPageSQL = `SELECT ` + driftColumns + taskActivitySQL + ` FROM tasks t
	WHERE t.state IN ('researching','developing')
	  AND ` + taskActivitySQL + ` < $1::timestamptz
	  AND t.id > $2
	  AND ($3::text IS NULL OR t.project = $3)
	ORDER BY t.id LIMIT $4`

// hygieneMergedWithoutDeployPageSQL pages merged tasks without deploy.
// $1 is cutoff time, $2 is since cursor, $3 is id cursor, $4 is project (or NULL), $5 is limit.
const hygieneMergedWithoutDeployPageSQL = `SELECT ` + driftColumns + `COALESCE(t.completed_at,t.updated_at) FROM tasks t
	WHERE t.state='merged'
	  AND COALESCE(t.completed_at,t.updated_at)<$1
	  AND ($4::text IS NULL OR t.project = $4)
	  AND (COALESCE(t.completed_at,t.updated_at),t.id)>($2,$3)
	ORDER BY COALESCE(t.completed_at,t.updated_at),t.id LIMIT $5`

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

	var projArg *string
	if project != "" {
		projArg = &project
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TaskHygieneReport{}, err
	}
	defer tx.Rollback(ctx)

	// 1. Gather MergedPRNotMerged candidates (paged by keyset).
	var mechanicalMergedTasks []TaskDriftTask
	var afterID int64
	for {
		rows, err := tx.Query(ctx, hygieneMergedPRPageSQL, afterID, projArg, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene merged PR candidates: %w", err)
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan merged PR candidates: %w", err)
		}
		for i := range page {
			x := &page[i]
			if err := s.annotateOneDriftHold(ctx, tx, x, mergeAppliesFrom); err != nil {
				return TaskHygieneReport{}, err
			}
			if x.Mechanical {
				mechanicalMergedTasks = append(mechanicalMergedTasks, *x)
			} else {
				if len(rep.Findings.MergedPROpenTasks) < hygieneFindingsLimit {
					rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, *x)
				} else {
					rep.Findings.MergedPROpenTasksTruncated = true
				}
			}
		}
		if len(page) < hygienePageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}

	// 2. Gather AutoCloseHeldParents candidates (paged by keyset).
	var mechanicalAutoCloseParents []TaskDriftTask
	afterID = 0
	for {
		rows, err := tx.Query(ctx, hygieneAutoCloseHeldPageSQL, afterID, projArg, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene auto close candidates: %w", err)
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan auto close candidates: %w", err)
		}
		for i := range page {
			x := &page[i]
			if err := s.annotateOneDriftHold(ctx, tx, x, autoCloseAppliesFrom); err != nil {
				return TaskHygieneReport{}, err
			}
			if x.Mechanical && s.AutoCloseParent() {
				mechanicalAutoCloseParents = append(mechanicalAutoCloseParents, *x)
			} else {
				if len(rep.Findings.AutoCloseHeldParents) < hygieneFindingsLimit {
					rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, *x)
				} else {
					rep.Findings.AutoCloseHeldParentsTruncated = true
				}
			}
		}
		if len(page) < hygienePageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}

	// 3. Gather SettledOpenDecisions (paged by keyset).
	afterID = 0
	for {
		rows, err := tx.Query(ctx, hygieneSettledOpenPageSQL, DecisionRequestOpen, afterID, projArg, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene settled open decisions: %w", err)
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan settled open decisions: %w", err)
		}
		for _, x := range page {
			if len(rep.Findings.SettledOpenDecisions) < hygieneFindingsLimit {
				rep.Findings.SettledOpenDecisions = append(rep.Findings.SettledOpenDecisions, x)
			} else {
				rep.Findings.SettledOpenDecisionsTruncated = true
				break
			}
		}
		if len(page) < hygienePageSize || rep.Findings.SettledOpenDecisionsTruncated {
			break
		}
		afterID = page[len(page)-1].ID
	}

	// 4. Gather LiveAttemptsWithoutLease (idle started, paged by keyset).
	stalledCutoff := now.Add(-s.StartedStalledHours())
	afterID = 0
	for {
		rows, err := tx.Query(ctx, hygieneIdleStartedPageSQL, stalledCutoff, afterID, projArg, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene idle started: %w", err)
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan idle started: %w", err)
		}
		for _, x := range page {
			if len(rep.Findings.LiveAttemptsWithoutLease) < hygieneFindingsLimit {
				rep.Findings.LiveAttemptsWithoutLease = append(rep.Findings.LiveAttemptsWithoutLease, x)
			} else {
				rep.Findings.LiveAttemptsWithoutLeaseTruncated = true
				break
			}
		}
		if len(page) < hygienePageSize || rep.Findings.LiveAttemptsWithoutLeaseTruncated {
			break
		}
		afterID = page[len(page)-1].ID
	}

	// 5. Gather lagging decisions (paged by keyset).
	lagCutoff := now.Add(-DecisionDeliveryLagAge)
	afterReqID := int64(0)
	for {
		rows, err := tx.Query(ctx, hygieneLaggingDecisionsSQL, now, lagCutoff, projArg, afterReqID, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene lagging decisions: %w", err)
		}
		var page []TaskHygieneLaggingDecision
		for rows.Next() {
			var d TaskHygieneLaggingDecision
			if err := rows.Scan(&d.RequestID, &d.Destination, &d.TaskTitle, &d.DesiredVersion, &d.AppliedVersion, &d.Attempts, &d.NextAttemptAt, &d.LastErrorCode); err != nil {
				rows.Close()
				return TaskHygieneReport{}, fmt.Errorf("task hygiene scan lagging decision: %w", err)
			}
			d.NextAttemptAt = d.NextAttemptAt.UTC()
			page = append(page, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene lagging decisions rows: %w", err)
		}
		for _, d := range page {
			if len(rep.Findings.AnsweredDecisionsNotDelivered) < hygieneFindingsLimit {
				rep.Findings.AnsweredDecisionsNotDelivered = append(rep.Findings.AnsweredDecisionsNotDelivered, d)
			} else {
				rep.Findings.AnsweredDecisionsNotDeliveredTruncated = true
				break
			}
		}
		if len(page) < hygienePageSize || rep.Findings.AnsweredDecisionsNotDeliveredTruncated {
			break
		}
		afterReqID = page[len(page)-1].RequestID
	}

	// 6. Gather MergedWithoutDeploy (paged by keyset).
	mergedCutoff := now.Add(-time.Duration(DriftDefaultMergedDays) * 24 * time.Hour)
	afterTime, afterMergedID := time.Unix(0, 0).UTC(), int64(0)
	for {
		rows, err := tx.Query(ctx, hygieneMergedWithoutDeployPageSQL, mergedCutoff, afterTime, afterMergedID, projArg, hygienePageSize)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene merged without deploy: %w", err)
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return TaskHygieneReport{}, fmt.Errorf("task hygiene scan merged without deploy: %w", err)
		}
		for _, x := range page {
			if x.PR != "" {
				key, err := deployRecordKeyForPRTx(ctx, tx, x.PR)
				if err != nil {
					return TaskHygieneReport{}, err
				}
				if key != "" {
					continue
				}
			}
			if len(rep.Findings.MergedWithoutDeploy) < hygieneFindingsLimit {
				rep.Findings.MergedWithoutDeploy = append(rep.Findings.MergedWithoutDeploy, x)
			} else {
				rep.Findings.MergedWithoutDeployTruncated = true
				break
			}
		}
		if len(page) < hygienePageSize || rep.Findings.MergedWithoutDeployTruncated {
			break
		}
		last := page[len(page)-1]
		afterTime, afterMergedID = last.Since, last.ID
	}

	_ = tx.Rollback(ctx)

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
		if s.AutoCloseParent() {
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
		}
	} else {
		// In apply mode, execute the mechanical closes under the level-2 task mutation lock.
		txMut, err := s.pool.Begin(ctx)
		if err != nil {
			return TaskHygieneReport{}, err
		}
		defer txMut.Rollback(ctx)

		if _, err := txMut.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
			return TaskHygieneReport{}, err
		}

		for _, cand := range mechanicalMergedTasks {
			var t Task
			if err := scanTask(txMut.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, cand.ID), &t); err != nil {
				return TaskHygieneReport{}, err
			}
			if project != "" && (t.Project == nil || *t.Project != project) {
				continue
			}
			h, err := taskHoldsTx(ctx, txMut, t.ID, "")
			if err != nil {
				return TaskHygieneReport{}, err
			}
			gate, err := s.closeGateTx(ctx, txMut, t)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			if !driftMechanical(h, gate, t.State, mergeAppliesFrom) {
				rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, cand)
				continue
			}
			pr := t.Refs.PR
			if pr == "" {
				pr, _ = heldMergePRTx(ctx, txMut, t.ID)
			}
			if pr == "" {
				_ = txMut.QueryRow(ctx, `SELECT COALESCE(NULLIF(t.refs->>'pr',''), c.pr_url, `+prURLSQL+`) FROM tasks t JOIN change_tasks ct ON ct.task_id=t.id AND ct.relation='implements' JOIN changes c ON c.id=ct.change_id AND c.state='merged' AND c.reverted_by_change_id IS NULL WHERE t.id=$1 ORDER BY c.id DESC LIMIT 1`, t.ID).Scan(&pr)
			}
			if pr == "" {
				rep.Findings.MergedPROpenTasks = append(rep.Findings.MergedPROpenTasks, cand)
				continue
			}
			fromState := t.State
			toState := StateMerged
			if canCompleteKind(t.Kind) {
				toState = StateCompleted
			}
			updated, err := s.transitionTaskTx(ctx, txMut, t.ID, TransitionInput{
				To:       toState,
				By:       "hk.hygiene",
				Note:     "hygiene: mechanical close of merged PR",
				Refs:     &TaskRefs{PR: pr},
				Evidence: pr,
			}, false)
			if err != nil {
				return TaskHygieneReport{}, err
			}
			rep.MechanicalCloses = append(rep.MechanicalCloses, TaskHygieneClose{
				TaskID:    t.ID,
				Title:     t.Title,
				Lane:      t.Lane,
				Kind:      t.Kind,
				FromState: fromState,
				ToState:   updated.State,
				Rule:      "merged_pr",
				PR:        pr,
			})
		}

		if s.AutoCloseParent() {
			for _, cand := range mechanicalAutoCloseParents {
				var p Task
				if err := scanTask(txMut.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, cand.ID), &p); err != nil {
					return TaskHygieneReport{}, err
				}
				if project != "" && (p.Project == nil || *p.Project != project) {
					continue
				}
				h, err := taskHoldsTx(ctx, txMut, p.ID, "")
				if err != nil {
					return TaskHygieneReport{}, err
				}
				gate, err := s.closeGateTx(ctx, txMut, p)
				if err != nil {
					return TaskHygieneReport{}, err
				}
				if !driftMechanical(h, gate, p.State, autoCloseAppliesFrom) {
					rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, cand)
					continue
				}
				var childID int64
				var childState string
				err = txMut.QueryRow(ctx, `SELECT id, state FROM tasks WHERE parent_id=$1 AND state IN ('completed','merged','deployed') ORDER BY updated_at DESC LIMIT 1`, p.ID).Scan(&childID, &childState)
				if err != nil {
					rep.Findings.AutoCloseHeldParents = append(rep.Findings.AutoCloseHeldParents, cand)
					continue
				}
				fromState := p.State
				closed, nextP, err := s.autoCloseOneParentTx(ctx, txMut, p, childID, childState, now)
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
						if err := s.autoCloseTx(ctx, txMut, nextP, "hk", now); err != nil {
							return TaskHygieneReport{}, err
						}
					}
				}
			}
		}

		if err := txMut.Commit(ctx); err != nil {
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

func scanDriftTasks(rows pgx.Rows) ([]TaskDriftTask, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaskDriftTask, error) {
		var x TaskDriftTask
		err := row.Scan(&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.PR, &x.DecisionRequest, &x.Since)
		if err != nil {
			return TaskDriftTask{}, err
		}
		x.Since = x.Since.UTC()
		return x, nil
	})
}

func (s *Store) annotateOneDriftHold(ctx context.Context, tx pgx.Tx, x *TaskDriftTask, applies func(state string) bool) error {
	var t Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, x.ID), &t); err != nil {
		return err
	}
	h, err := taskHoldsTx(ctx, tx, x.ID, "")
	if err != nil {
		return err
	}
	gate, err := s.closeGateTx(ctx, tx, t)
	if err != nil {
		return err
	}
	x.OpenPRs, x.RevertedPRs, x.OpenSubTasks = h.OpenPRs, h.RevertedPRs, h.OpenSubTasks
	x.Hold = h.mergeReason()
	if x.Hold == "" {
		x.Hold = gate
	}
	x.Mechanical = driftMechanical(h, gate, t.State, applies)
	return nil
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

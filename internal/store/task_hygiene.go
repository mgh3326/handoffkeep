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

// TaskHygieneClose records a net task state change in one mechanical command.
// Apply reports include every changed ancestor, including merged-to-deployed.
// Dry-run rows are direct candidate predictions in the discovery snapshot;
// they do not simulate newly enabled closes or cascading transitions.
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

// All predicates below are composed with FROM tasks, without an alias.
// In particular taskActivitySQL requires the relation name tasks.
var hygieneMergedPredicate = `state NOT IN (` + settledStatesSQL + `)
 AND (
   EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
     WHERE ct.task_id=tasks.id AND ct.relation='implements'
       AND c.state='merged' AND c.reverted_by_change_id IS NULL)
   OR (COALESCE(refs->>'pr','')<>'' AND (
     EXISTS (SELECT 1 FROM changes c WHERE c.state='merged'
       AND (` + prURLSQL + `)=tasks.refs->>'pr')
     OR EXISTS (SELECT 1 FROM tasks m WHERE m.id<>tasks.id
       AND m.state IN ('merged','deployed') AND m.refs->>'pr'=tasks.refs->>'pr')))
 ) AND NOT EXISTS (SELECT 1 FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
   WHERE ` + unresolvedRevertSQL("tasks.id") + `)`

var hygieneParentPredicate = `state IN (` + autoCloseStatesSQL + `)
 AND EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=tasks.id
   AND k.state IN ('completed','merged','deployed'))
 AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=tasks.id
   AND k.state NOT IN (` + settledStatesSQL + `))
 AND (kind IN ('decide','ops','verify','research')
   OR EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id=tasks.id
     AND k.state IN ('merged','deployed')))`

// $1 now, $2 cutoff, $3 project, ($4,$5) full primary-key cursor, $6 limit.
const hygieneLaggingDecisionsSQL = `SELECT n.request_id,n.destination,
 COALESCE(t.title,''),n.desired_version,n.applied_version,n.attempts,
 n.next_attempt_at,n.last_error_code
 FROM decision_deliveries n JOIN decision_requests d ON d.id=n.request_id
 LEFT JOIN tasks t ON t.id=d.task_id
 WHERE n.state='pending' AND n.applied_version<>` + decisionEffectiveStatusSQL + `
 AND n.next_attempt_at<$2::timestamptz
 AND ($3::text IS NULL OR t.project=$3)
 AND (n.request_id,n.destination)>($4::bigint,$5::text)
 ORDER BY n.request_id,n.destination LIMIT $6`

type hygieneRule uint8

const (
	hygieneMergedRule hygieneRule = iota
	hygieneParentRule
)

func (r hygieneRule) name() string {
	if r == hygieneMergedRule {
		return "merged_pr"
	}
	return "auto_close_parent"
}

func (r hygieneRule) predicate() string {
	if r == hygieneMergedRule {
		return hygieneMergedPredicate
	}
	return hygieneParentPredicate
}

type hygieneCandidate struct {
	rule hygieneRule
	id   int64
}

// A discovered ID is only a hint. This decision is rebuilt in the execution
// transaction, under the mutation lock and task row lock.
type hygieneDecision struct {
	task       Task
	finding    TaskDriftTask
	present    bool
	ready      bool
	target     string
	pr         string
	childID    int64
	childState string
}

// hygieneAppend is the only insertion path for bounded findings. Truncated
// means that an additional qualifying row was actually observed.
func hygieneAppend[T any](dst *[]T, truncated *bool, x T) {
	if len(*dst) < hygieneFindingsLimit {
		*dst = append(*dst, x)
	} else {
		*truncated = true
	}
}

func hygieneAddFinding(rep *TaskHygieneReport, rule hygieneRule, x TaskDriftTask) {
	x.Mechanical = false
	if rule == hygieneMergedRule {
		hygieneAppend(&rep.Findings.MergedPROpenTasks, &rep.Findings.MergedPROpenTasksTruncated, x)
	} else {
		hygieneAppend(&rep.Findings.AutoCloseHeldParents, &rep.Findings.AutoCloseHeldParentsTruncated, x)
	}
}

// TaskHygiene discovers discrepancies in one read-only snapshot, then handles
// each mechanical candidate in its own transaction. A domain refusal rolls
// back that entire candidate, including any links or ancestor events, and is
// reported as a finding. Unexpected errors stop the sweep and return the
// report of already committed commands alongside the error.
//
// AsOf is the observation/age-cutoff clock. Existing transitionTaskTx and
// SettleMerged own their mutation clocks; it is not a transaction timestamp.
// Project scopes candidate roots; existing hierarchy rules may close ancestors
// in other projects, and those effects are included in the apply report.
func (s *Store) TaskHygiene(ctx context.Context, now time.Time, project string, dryRun bool) (TaskHygieneReport, error) {
	if project != "" && !validName(project) {
		return TaskHygieneReport{}, errors.New("invalid task hygiene: project must be a project name")
	}
	now = now.UTC()
	rep := TaskHygieneReport{
		AsOf: now, DryRun: dryRun, Project: project,
		MechanicalCloses: []TaskHygieneClose{},
		Findings: TaskHygieneFindings{
			MergedPROpenTasks: []TaskDriftTask{}, AutoCloseHeldParents: []TaskDriftTask{},
			SettledOpenDecisions: []TaskDriftTask{}, LiveAttemptsWithoutLease: []TaskDriftTask{},
			AnsweredDecisionsNotDelivered: []TaskHygieneLaggingDecision{}, MergedWithoutDeploy: []TaskDriftTask{},
		},
	}
	var projectArg *string
	if project != "" {
		projectArg = &project
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx)
	candidates, err := s.hygieneDiscoverTx(ctx, tx, now, projectArg, &rep)
	if err != nil {
		return rep, fmt.Errorf("task hygiene discover: %w", err)
	}
	if dryRun {
		// Preview is read-only and snapshot-based. Overlapping rules produce one
		// direct prediction per task, in the same rule priority as execution.
		seen := map[int64]bool{}
		for _, candidate := range candidates {
			if seen[candidate.id] {
				continue
			}
			d, err := s.hygieneDecideTx(ctx, tx, candidate, project, false)
			if err != nil {
				return rep, fmt.Errorf("task hygiene preview task %d: %w", candidate.id, err)
			}
			if !d.present {
				continue
			}
			if !d.ready {
				hygieneAddFinding(&rep, candidate.rule, d.finding)
				continue
			}
			seen[candidate.id] = true
			rep.MechanicalCloses = append(rep.MechanicalCloses, hygieneClose(d.task, d.target, d.pr, candidate.rule.name()))
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return rep, err
	}
	if !dryRun {
		for _, candidate := range candidates {
			effects, finding, err := s.hygieneExecute(ctx, candidate, project, now)
			if err != nil {
				return rep, fmt.Errorf("task hygiene %s task %d: %w", candidate.rule.name(), candidate.id, err)
			}
			if finding != nil {
				hygieneAddFinding(&rep, candidate.rule, *finding)
			}
			// hygieneExecute returns effects only after a successful commit.
			rep.MechanicalCloses = append(rep.MechanicalCloses, effects...)
		}
	}
	settle, err := s.SettleMerged(ctx, project, dryRun)
	if err != nil {
		return rep, fmt.Errorf("task hygiene settle-merged: %w", err)
	}
	rep.SettleMerged = &settle
	return rep, nil
}

// hygieneWalkTasksTx applies project filtering before pagination. Discovery
// has no report cap. A visitor may stop after observing the first omitted
// qualifying finding. Extra SQL arguments start at $4.
func hygieneWalkTasksTx(ctx context.Context, tx pgx.Tx, project *string, predicate, since string, visit func(TaskDriftTask) (bool, error), extra ...any) error {
	sql := `SELECT ` + driftColumns + since + ` FROM tasks WHERE (` + predicate + `)
   AND id>$1 AND ($2::text IS NULL OR project=$2) ORDER BY id LIMIT $3`
	var after int64
	for {
		args := []any{after, project, hygienePageSize}
		args = append(args, extra...)
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		page, err := scanDriftTasks(rows)
		if err != nil {
			return err
		}
		for _, x := range page {
			stop, err := visit(x)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
		if len(page) < hygienePageSize {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Store) hygieneDiscoverTx(ctx context.Context, tx pgx.Tx, now time.Time, project *string, rep *TaskHygieneReport) ([]hygieneCandidate, error) {
	var candidates []hygieneCandidate
	for _, rule := range []hygieneRule{hygieneMergedRule, hygieneParentRule} {
		err := hygieneWalkTasksTx(ctx, tx, project, rule.predicate(), "updated_at", func(x TaskDriftTask) (bool, error) {
			candidates = append(candidates, hygieneCandidate{rule: rule, id: x.ID})
			return false, nil
		})
		if err != nil {
			return nil, err
		}
	}
	err := hygieneWalkTasksTx(ctx, tx, project, `state IN (`+settledStatesSQL+`) AND refs->'decision_request'->>'status'=$4`, "updated_at", func(x TaskDriftTask) (bool, error) {
		hygieneAppend(&rep.Findings.SettledOpenDecisions, &rep.Findings.SettledOpenDecisionsTruncated, x)
		return rep.Findings.SettledOpenDecisionsTruncated, nil
	}, DecisionRequestOpen)
	if err != nil {
		return nil, err
	}
	err = hygieneWalkTasksTx(ctx, tx, project, `state IN ('researching','developing') AND `+taskActivitySQL+`<$4::timestamptz`, taskActivitySQL, func(x TaskDriftTask) (bool, error) {
		hygieneAppend(&rep.Findings.LiveAttemptsWithoutLease, &rep.Findings.LiveAttemptsWithoutLeaseTruncated, x)
		return rep.Findings.LiveAttemptsWithoutLeaseTruncated, nil
	}, now.Add(-s.StartedStalledHours()))
	if err != nil {
		return nil, err
	}
	if err = hygieneReadLaggingTx(ctx, tx, now, project, rep); err != nil {
		return nil, err
	}
	err = hygieneWalkTasksTx(ctx, tx, project, `state='merged' AND COALESCE(completed_at,updated_at)<$4::timestamptz`, "COALESCE(completed_at,updated_at)", func(x TaskDriftTask) (bool, error) {
		if x.PR != "" {
			key, err := deployRecordKeyForPRTx(ctx, tx, x.PR)
			if err != nil {
				return false, err
			}
			if key != "" {
				return false, nil
			}
		}
		hygieneAppend(&rep.Findings.MergedWithoutDeploy, &rep.Findings.MergedWithoutDeployTruncated, x)
		return rep.Findings.MergedWithoutDeployTruncated, nil
	}, now.Add(-time.Duration(DriftDefaultMergedDays)*24*time.Hour))
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func hygieneReadLaggingTx(ctx context.Context, tx pgx.Tx, now time.Time, project *string, rep *TaskHygieneReport) error {
	var afterID int64
	afterDestination := ""
	for {
		rows, err := tx.Query(ctx, hygieneLaggingDecisionsSQL, now, now.Add(-DecisionDeliveryLagAge), project, afterID, afterDestination, hygienePageSize)
		if err != nil {
			return err
		}
		page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaskHygieneLaggingDecision, error) {
			var x TaskHygieneLaggingDecision
			err := row.Scan(&x.RequestID, &x.Destination, &x.TaskTitle, &x.DesiredVersion, &x.AppliedVersion, &x.Attempts, &x.NextAttemptAt, &x.LastErrorCode)
			x.NextAttemptAt = x.NextAttemptAt.UTC()
			return x, err
		})
		if err != nil {
			return err
		}
		for _, x := range page {
			hygieneAppend(&rep.Findings.AnsweredDecisionsNotDelivered, &rep.Findings.AnsweredDecisionsNotDeliveredTruncated, x)
			if rep.Findings.AnsweredDecisionsNotDeliveredTruncated {
				return nil
			}
		}
		if len(page) < hygienePageSize {
			return nil
		}
		last := page[len(page)-1]
		afterID, afterDestination = last.RequestID, last.Destination
	}
}

func scanDriftTasks(rows pgx.Rows) ([]TaskDriftTask, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaskDriftTask, error) {
		var x TaskDriftTask
		err := row.Scan(&x.ID, &x.Lane, &x.Title, &x.Kind, &x.State, &x.PR, &x.DecisionRequest, &x.Since)
		x.Since = x.Since.UTC()
		return x, err
	})
}

// hygieneVerifiedPRTx trusts merged, unreverted changes only. An old refs.pr
// is preferred only if it passes that test. Other eligible evidence is a
// linked implements change or a recorded held merge of this task.
func hygieneVerifiedPRTx(ctx context.Context, tx pgx.Tx, t Task) (string, error) {
	var pr string
	err := tx.QueryRow(ctx, `SELECT `+prURLSQL+` FROM changes c
   WHERE c.state='merged' AND c.reverted_by_change_id IS NULL
   AND ((`+prURLSQL+`)=$2
     OR EXISTS(SELECT 1 FROM change_tasks ct WHERE ct.change_id=c.id
       AND ct.task_id=$1 AND ct.relation='implements')
     OR EXISTS(SELECT 1 FROM task_events e WHERE e.task_id=$1
       AND e.source=$3 AND e.field=$4 AND NOT e.applied
       AND e.source_ref=(`+prURLSQL+`)))
   ORDER BY ((`+prURLSQL+`)=$2) DESC,c.id DESC LIMIT 1`, t.ID, t.Refs.PR, SourceMergewakeMerge, heldMergeField).Scan(&pr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return pr, err
}

func (s *Store) hygieneDecideTx(ctx context.Context, tx pgx.Tx, c hygieneCandidate, project string, lock bool) (hygieneDecision, error) {
	var d hygieneDecision
	sql := `SELECT ` + taskColumns + ` FROM tasks WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	if err := scanTask(tx.QueryRow(ctx, sql, c.id), &d.task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return d, nil
		}
		return d, err
	}
	t := d.task
	if project != "" && (t.Project == nil || *t.Project != project) {
		return d, nil
	}
	// Recheck the discrepancy, not only its holds. A PR can have been reverted
	// or a parent can have acquired an unsettled child since discovery.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND (`+c.rule.predicate()+`))`, t.ID).Scan(&d.present); err != nil {
		return d, err
	}
	if !d.present {
		return d, nil
	}
	d.finding = TaskDriftTask{ID: t.ID, Lane: t.Lane, Title: t.Title, Kind: t.Kind, State: t.State, PR: t.Refs.PR, Since: t.UpdatedAt.UTC()}
	if t.Refs.DecisionRequest != nil {
		d.finding.DecisionRequest = t.Refs.DecisionRequest.ID
	}
	h, err := taskHoldsTx(ctx, tx, t.ID, "")
	if err != nil {
		return d, err
	}
	gate, err := s.closeGateTx(ctx, tx, t)
	if err != nil {
		return d, err
	}
	d.finding.OpenPRs, d.finding.RevertedPRs, d.finding.OpenSubTasks = h.OpenPRs, h.RevertedPRs, h.OpenSubTasks
	d.finding.Hold = h.mergeReason()
	if d.finding.Hold == "" {
		d.finding.Hold = gate
	}
	applies := mergeAppliesFrom
	if c.rule == hygieneParentRule {
		applies = autoCloseAppliesFrom
	}
	if !driftMechanical(h, gate, t.State, applies) {
		if d.finding.Hold == "" {
			d.finding.Hold = "automatic close does not apply from " + t.State
		}
		return d, nil
	}
	if c.rule == hygieneParentRule {
		if !s.AutoCloseParent() {
			d.finding.Hold = "auto_close_parent is disabled"
			return d, nil
		}
		roll, err := subTaskRollupTx(ctx, tx, t.ID)
		if err != nil {
			return d, err
		}
		d.target = roll.closeTarget()
		if d.target == "" || (d.target == StateCompleted && !canCompleteKind(t.Kind)) {
			d.finding.Hold = "sub-task rollup does not permit this parent to close"
			return d, nil
		}
		err = tx.QueryRow(ctx, `SELECT id,state FROM tasks WHERE parent_id=$1
     AND state IN ('completed','merged','deployed')
     ORDER BY updated_at DESC,id DESC LIMIT 1`, t.ID).Scan(&d.childID, &d.childState)
		if errors.Is(err, pgx.ErrNoRows) {
			d.finding.Hold = "no settled child with work remains"
			return d, nil
		}
		if err != nil {
			return d, err
		}
		d.pr = t.Refs.PR
		if d.target == StateMerged && d.pr == "" {
			d.pr, err = heldMergePRTx(ctx, tx, t.ID)
			if err != nil {
				return d, err
			}
		}
	} else {
		d.pr, err = hygieneVerifiedPRTx(ctx, tx, t)
		if err != nil {
			return d, err
		}
		if d.pr == "" {
			d.finding.Hold = "no merged, unreverted change verifies the closing PR"
			return d, nil
		}
		d.target = StateMerged
		if canCompleteKind(t.Kind) {
			d.target = StateCompleted
		}
		if !taskTransitionAllowed(t.State, d.target) && !(d.target == StateMerged && deliveredOpenStates[t.State]) {
			d.finding.Hold = "the task transition graph does not permit the close"
			return d, nil
		}
	}
	d.ready = true
	d.finding.Mechanical = true
	return d, nil
}

// Each command owns its transaction. Expected domain refusal must roll it
// back even if PostgreSQL is still usable: transitionTaskTx may have inserted
// an implements link before returning ErrTaskConflict.
func (s *Store) hygieneExecute(ctx context.Context, c hygieneCandidate, project string, now time.Time) ([]TaskHygieneClose, *TaskDriftTask, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return nil, nil, err
	}
	d, err := s.hygieneDecideTx(ctx, tx, c, project, true)
	if err != nil {
		return nil, nil, err
	}
	if !d.present {
		return nil, nil, nil
	}
	if !d.ready {
		return nil, &d.finding, nil
	}
	before, err := hygieneAncestorTasksTx(ctx, tx, d.task)
	if err != nil {
		return nil, nil, err
	}
	if c.rule == hygieneMergedRule {
		_, err = s.transitionTaskTx(ctx, tx, c.id, TransitionInput{
			To: d.target, By: "hk.hygiene", Note: "hygiene: mechanical close of merged PR",
			Refs: &TaskRefs{PR: d.pr}, Evidence: d.pr,
		}, false)
	} else if s.AutoCloseParent() {
		var closed bool
		var next Task
		closed, next, err = s.autoCloseOneParentTx(ctx, tx, d.task, d.childID, d.childState, now)
		if err == nil && closed {
			err = s.autoCloseTx(ctx, tx, next, "hk", now)
		}
	} else {
		d.finding.Mechanical = false
		d.finding.Hold = "auto_close_parent is disabled"
		return nil, &d.finding, nil
	}
	if err != nil {
		if errors.Is(err, ErrTaskConflict) {
			d.finding.Mechanical = false
			d.finding.Hold = err.Error() // Presentation only; classification uses errors.Is.
			return nil, &d.finding, nil
		}
		return nil, nil, err
	}
	effects, err := hygieneNetEffectsTx(ctx, tx, before, c)
	if err != nil {
		return nil, nil, err
	}
	if len(effects) == 0 {
		// In particular roll back any not-applied auto-close event. A recheck
		// refusal is a finding, never a claimed close.
		d.finding.Mechanical = false
		d.finding.Hold = "automatic close was not applied after recheck"
		return nil, &d.finding, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return effects, nil, nil
}

// These helpers snapshot only the command's task and its ancestor chain.
// The mutation lock stabilizes topology and state transitions; row locks
// protect the snapshots against the permitted single-row task editors.
// Current transitionTaskTx cannot change any other task for these non-triage
// close inputs. If that helper's effect scope grows, change this contract too.
func hygieneAncestorTasksTx(ctx context.Context, tx pgx.Tx, root Task) ([]Task, error) {
	out := []Task{root}
	child := root
	for child.ParentID != nil {
		var p Task
		if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, *child.ParentID), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
		child = p
	}
	return out, nil
}

func hygieneNetEffectsTx(ctx context.Context, tx pgx.Tx, before []Task, c hygieneCandidate) ([]TaskHygieneClose, error) {
	effects := []TaskHygieneClose{}
	for _, old := range before {
		var current Task
		if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, old.ID), &current); err != nil {
			return nil, err
		}
		if old.State == current.State {
			continue
		}
		if !IsSettledState(current.State) {
			return nil, fmt.Errorf("task hygiene: unexpected resulting state %q for task %d", current.State, current.ID)
		}
		rule := "auto_close_parent"
		if old.ID == c.id {
			rule = c.rule.name()
		}
		effects = append(effects, hygieneClose(old, current.State, current.Refs.PR, rule))
	}
	return effects, nil
}

func hygieneClose(t Task, to, pr, rule string) TaskHygieneClose {
	return TaskHygieneClose{TaskID: t.ID, Title: t.Title, Lane: t.Lane, Kind: t.Kind,
		FromState: t.State, ToState: to, Rule: rule, PR: pr}
}

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The settle-merged reasons. Every merged task in scope gets exactly one.
const (
	// SettleDeployed: every implements change is contained on every target of
	// its deploy stage, and an included successful deploy carries it.
	SettleDeployed = "deployed"
	// SettleNoMergeCommit: the task has no live implements change, or one of
	// them has no merge commit recorded.
	SettleNoMergeCommit = "no_merge_commit"
	// SettleNoDeployStage: no service of the task's changes' repos has a
	// deploy flow (deploy_mode other than none).
	SettleNoDeployStage = "no_deploy_stage"
	// SettleNotYetDeployed: a deploy stage exists but a change is not yet
	// contained on every target, or no successful deploy includes it.
	SettleNotYetDeployed = "not_yet_deployed"
)

// settleReasons lists the reasons in report order.
var settleReasons = []string{SettleDeployed, SettleNoMergeCommit, SettleNoDeployStage, SettleNotYetDeployed}

// SettleMergedReport is one settle-merged sweep. Counts and Tasks carry the
// same figures per reason; Tasks lists task ids in ascending order.
type SettleMergedReport struct {
	DryRun  bool               `json:"dry_run"`
	Project string             `json:"project,omitempty"`
	Counts  map[string]int     `json:"counts"`
	Tasks   map[string][]int64 `json:"tasks"`
}

// SettleMerged applies the deploy task rule (deployedTasksTx) to the merged
// tasks of project (every project when empty): a task whose implements
// changes are all deployed moves to deployed with the newest successful
// included attempt as evidence (source hk.deploy). Every other task stays
// merged and is counted under its reason. dryRun decides and writes nothing.
// A re-run finds nothing new to move, so it changes nothing.
func (s *Store) SettleMerged(ctx context.Context, project string, dryRun bool) (SettleMergedReport, error) {
	if project != "" && !validName(project) {
		return SettleMergedReport{}, errors.New("invalid settle-merged: project must be a project name")
	}
	rep := SettleMergedReport{DryRun: dryRun, Project: project, Counts: map[string]int{}, Tasks: map[string][]int64{}}
	for _, r := range settleReasons {
		rep.Counts[r] = 0
		rep.Tasks[r] = []int64{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SettleMergedReport{}, err
	}
	defer tx.Rollback(ctx)
	// Level 2 before the task rows (lockorder.go), as FinishDeploy takes it.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return SettleMergedReport{}, err
	}
	var projectArg *string
	if project != "" {
		projectArg = &project
	}
	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE state=$1 AND ($2::text IS NULL OR project=$2) ORDER BY id FOR UPDATE`, StateMerged, projectArg)
	if err != nil {
		return SettleMergedReport{}, err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Task, error) {
		var x Task
		return x, scanTask(row, &x)
	})
	if err != nil {
		return SettleMergedReport{}, err
	}
	now := time.Now().UTC()
	for _, x := range tasks {
		reason, attempt, err := s.settleReasonTx(ctx, tx, x.ID)
		if err != nil {
			return SettleMergedReport{}, err
		}
		rep.Counts[reason]++
		rep.Tasks[reason] = append(rep.Tasks[reason], x.ID)
		if reason != SettleDeployed || dryRun {
			continue
		}
		ev := sourceEvent{Source: SourceDeploy, SourceRef: "deploy:" + attempt, At: now, Note: "settle-merged: every implements change is deployed in attempt " + attempt}
		if _, err = s.applySourceEventTx(ctx, tx, x, ev, now); err != nil {
			return SettleMergedReport{}, err
		}
	}
	if dryRun {
		return rep, nil
	}
	return rep, tx.Commit(ctx)
}

// settleReasonTx decides one merged task. It returns SettleDeployed with the
// attempt id that carries the task, or the reason it stays merged. The rules
// are the ones deployedTasksTx and PendingChanges use: changeDeployedSQL for
// containment, and an included successful deploy for the evidence.
func (s *Store) settleReasonTx(ctx context.Context, tx pgx.Tx, taskID int64) (string, string, error) {
	var linked, noSHA int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE c.merge_sha IS NULL)
FROM change_tasks ct JOIN changes c ON c.id=ct.change_id
WHERE ct.task_id=$1 AND ct.relation=$2 AND c.reverted_by_change_id IS NULL`, taskID, ChangeImplements).Scan(&linked, &noSHA); err != nil {
		return "", "", err
	}
	if linked == 0 || noSHA > 0 {
		return SettleNoMergeCommit, "", nil
	}
	var stage bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM change_tasks ct
JOIN changes c ON c.id=ct.change_id JOIN services s ON s.repo=c.repo
WHERE ct.task_id=$1 AND ct.relation=$2 AND c.reverted_by_change_id IS NULL AND s.deploy_mode<>'none')`, taskID, ChangeImplements).Scan(&stage); err != nil {
		return "", "", err
	}
	if !stage {
		return SettleNoDeployStage, "", nil
	}
	var contained bool
	if err := tx.QueryRow(ctx, `SELECT bool_and(`+changeDeployedSQL()+`) IS TRUE
FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=$1 AND ct.relation=$2 AND c.reverted_by_change_id IS NULL`, taskID, ChangeImplements).Scan(&contained); err != nil {
		return "", "", err
	}
	if !contained {
		return SettleNotYetDeployed, "", nil
	}
	var attempt string
	err := tx.QueryRow(ctx, `SELECT d.attempt_id FROM change_tasks ct
JOIN changes c ON c.id=ct.change_id
JOIN deploy_changes dc ON dc.change_id=c.id AND dc.included
JOIN deploys d ON d.id=dc.deploy_id
WHERE ct.task_id=$1 AND ct.relation=$2 AND c.reverted_by_change_id IS NULL AND d.state='success'
ORDER BY d.finished_at DESC, d.id DESC LIMIT 1`, taskID, ChangeImplements).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SettleNotYetDeployed, "", nil
	}
	if err != nil {
		return "", "", err
	}
	return SettleDeployed, attempt, nil
}

package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const changeColumnsMySQL = `id,repo,pr_number,state,head_sha,base_sha,merge_sha,opened_at,merged_at,reverted_by_change_id,priority,hold_reason,depends_on_change_id,depends_on_state,created_at,updated_at,
	draft,gate_state,CAST(NULLIF(LEAST(COALESCE(state_observed_at,'9999-12-31 23:59:59.999999'),COALESCE(checks_observed_at,'9999-12-31 23:59:59.999999'),COALESCE(threads_observed_at,'9999-12-31 23:59:59.999999'),COALESCE(findings_observed_at,'9999-12-31 23:59:59.999999'),COALESCE(gate_decided_at,'9999-12-31 23:59:59.999999')),'9999-12-31 23:59:59.999999') AS DATETIME(6)),
	(SELECT COUNT(*) FROM change_threads t WHERE t.change_id=changes.id AND NOT t.resolved AND NOT t.gone),
	(SELECT COUNT(*) FROM change_findings f LEFT JOIN change_threads t ON t.change_id=f.change_id AND t.thread_id=COALESCE(f.thread_id,f.ext_id)
	 WHERE f.change_id=changes.id AND (CASE WHEN t.thread_id IS NOT NULL THEN IF(t.resolved OR t.gone,'resolved','open') WHEN f.reviewed_head=changes.head_sha THEN 'open' ELSE 'superseded' END)='open')`

func scanChangeMySQL(r row) (Change, error) {
	var c Change
	err := r.Scan(&c.ID, &c.Repo, &c.PRNumber, &c.State, &c.HeadSHA, &c.BaseSHA, &c.MergeSHA, &c.OpenedAt, &c.MergedAt, &c.RevertedByChangeID, &c.Priority, &c.HoldReason, &c.DependsOnChangeID, &c.DependsOnState, &c.CreatedAt, &c.UpdatedAt, &c.Draft, &c.GateState, &c.FreshnessMin, &c.OpenThreads, &c.OpenFindings)
	if err == nil {
		c.PR = changeURL(c.Repo, c.PRNumber)
		c.Tasks, c.Reviews, c.Verifications = []ChangeTask{}, []ChangeReview{}, []Verification{}
	}
	return c, err
}

func (s *Store) listChangesWhereMySQL(ctx context.Context, where string, afterID *int64, limit int, args ...any) ([]Change, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	if afterID != nil {
		if *afterID < 0 {
			return nil, errors.New("invalid change query")
		}
		args = append(args, *afterID)
		where += ` AND id>?`
	}
	query := `SELECT ` + changeColumnsMySQL + ` FROM changes WHERE ` + where + ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	changes, err := collectRows(rows, scanChangeMySQL)
	if err != nil || len(changes) == 0 {
		return []Change{}, err
	}
	ids := make([]int64, len(changes))
	positions := make(map[int64]int, len(changes))
	for i := range changes {
		ids[i] = changes[i].ID
		positions[changes[i].ID] = i
	}
	marks := placeholders(len(ids))
	rows, err = s.db().Query(ctx, `SELECT change_id,task_id,relation FROM change_tasks WHERE change_id IN (`+marks+`) ORDER BY change_id,task_id`, intArgs(ids)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var changeID int64
		var task ChangeTask
		if err = rows.Scan(&changeID, &task.TaskID, &task.Relation); err != nil {
			rows.Close()
			return nil, err
		}
		changes[positions[changeID]].Tasks = append(changes[positions[changeID]].Tasks, task)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.db().Query(ctx, `SELECT change_id,id,reviewed_head,reviewed_base,patch_id,round,pass,lens,reviewer_job_id,family,profile,model,harness,effort,executor,fallback,policy_revision,contract_source,verdict,critical,high,medium,low,started_at,finished_at,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest FROM reviews WHERE change_id IN (`+marks+`) ORDER BY id`, intArgs(ids)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var changeID int64
		var review ChangeReview
		if err = rows.Scan(&changeID, &review.ID, &review.ReviewedHead, &review.ReviewedBase, &review.PatchID, &review.Round, &review.Pass, &review.Lens, &review.ReviewerJobID, &review.Family, &review.Profile, &review.Model, &review.Harness, &review.Effort, &review.Executor, &review.Fallback, &review.PolicyRevision, &review.ContractSource, &review.Verdict, &review.Critical, &review.High, &review.Medium, &review.Low, &review.StartedAt, &review.FinishedAt, &review.RecordedAt, &review.Reviewer, &review.SameFamily, &review.Delta, &review.Quiet, &review.OutsideDiffBlocking, &review.PatchDigest); err != nil {
			rows.Close()
			return nil, err
		}
		changes[positions[changeID]].Reviews = append(changes[positions[changeID]].Reviews, review)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.db().Query(ctx, `SELECT `+verificationColumns+` FROM verifications WHERE change_id IN (`+marks+`) ORDER BY recorded_at DESC, id DESC`, intArgs(ids)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v Verification
		if err = scanVerification(rows, &v); err != nil {
			rows.Close()
			return nil, err
		}
		c := &changes[positions[v.ChangeID]]
		v.Current = c.HeadSHA != nil && *c.HeadSHA == v.HeadSHA
		c.Verifications = append(c.Verifications, v)
		if v.Current {
			current := v
			c.Verification = &current
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return changes, nil
}

func intArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (s *Store) setChangeMergeFlowMySQL(ctx context.Context, id int64, in ChangeMergeFlow) (Change, error) {
	if err := checkChangeMergeFlow(in); err != nil {
		return Change{}, err
	}
	now := time.Now().UTC()
	tx, release, err := s.begin(ctx)
	if err != nil {
		return Change{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("id", fmt.Sprint(changeAdvisoryLock)), false); err != nil {
		return Change{}, err
	}
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM changes WHERE id=? FOR UPDATE`, id).Scan(&locked); errors.Is(err, ErrNoRows) {
		return Change{}, ErrChangeNotFound
	} else if err != nil {
		return Change{}, err
	}
	var state *string
	if in.DependsOnChange != nil {
		target := *in.DependsOnChange
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM changes WHERE id=?)`, target).Scan(&exists); err != nil {
			return Change{}, err
		}
		if !exists {
			return Change{}, ErrChangeNotFound
		}
		var cycle bool
		err = tx.QueryRow(ctx, `WITH RECURSIVE chain(id) AS (SELECT ? UNION SELECT c.depends_on_change_id FROM changes c JOIN chain ON c.id=chain.id WHERE c.depends_on_change_id IS NOT NULL) SELECT EXISTS(SELECT 1 FROM chain WHERE id=?)`, target, id).Scan(&cycle)
		if err != nil {
			return Change{}, err
		}
		if cycle {
			return Change{}, fmt.Errorf("%w: change %d would depend on itself through change %d", ErrChangeDependencyCycle, id, target)
		}
		state = &in.DependsOnState
	}
	if _, err = tx.Exec(ctx, `UPDATE changes SET priority=?,hold_reason=?,depends_on_change_id=?,depends_on_state=?,updated_at=? WHERE id=?`, in.Priority, nullStr(in.HoldReason), in.DependsOnChange, state, now, id); err != nil {
		return Change{}, err
	}
	var out Change
	err = tx.QueryRow(ctx, `SELECT `+changeColumnsMySQL+` FROM changes WHERE id=?`, id).Scan(&out.ID, &out.Repo, &out.PRNumber, &out.State, &out.HeadSHA, &out.BaseSHA, &out.MergeSHA, &out.OpenedAt, &out.MergedAt, &out.RevertedByChangeID, &out.Priority, &out.HoldReason, &out.DependsOnChangeID, &out.DependsOnState, &out.CreatedAt, &out.UpdatedAt, &out.Draft, &out.GateState, &out.FreshnessMin, &out.OpenThreads, &out.OpenFindings)
	if errors.Is(err, ErrNoRows) {
		return Change{}, ErrChangeNotFound
	}
	if err != nil {
		return Change{}, err
	}
	out.PR = changeURL(out.Repo, out.PRNumber)
	out.Tasks, out.Reviews, out.Verifications = []ChangeTask{}, []ChangeReview{}, []Verification{}
	if err = tx.Commit(ctx); err != nil {
		return Change{}, err
	}
	return out, nil
}

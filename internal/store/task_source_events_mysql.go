package store

import (
	"context"
	"time"
)

// The MySQL side of task_source_events.go. The names carry the srcEv prefix so
// no other porter's helper can collide with them.

// refs->>'pr' and refs->>'job_id' of a tasks row. JSON_UNQUOTE of a JSON null
// is the string 'null' where PostgreSQL's ->> gives SQL NULL, so the value is
// read only when it is a JSON string. The JSON calls are pinned to
// utf8mb4_0900_bin: JSON strings carry utf8mb4_bin, which no longer mixes
// with the connection collation (design §11.4, hk 5169).
const (
	srcEvRefsPRMySQL    = `CASE WHEN JSON_TYPE(JSON_EXTRACT(refs,'$.pr')) COLLATE utf8mb4_0900_bin='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(refs,'$.pr')) COLLATE utf8mb4_0900_bin END`
	srcEvRefsJobIDMySQL = `CASE WHEN JSON_TYPE(JSON_EXTRACT(refs,'$.job_id')) COLLATE utf8mb4_0900_bin='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(refs,'$.job_id')) COLLATE utf8mb4_0900_bin END`
)

// srcEvDeliveredPRMySQL is taskDeliveredPRSQL for the tasks row aliased t: the
// task's own refs.pr, else the refs.pr of a "[bundle ...]" parent, else the empty string.
const srcEvDeliveredPRMySQL = `COALESCE(NULLIF(CASE WHEN JSON_TYPE(JSON_EXTRACT(t.refs,'$.pr')) COLLATE utf8mb4_0900_bin='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.pr')) COLLATE utf8mb4_0900_bin END,''), ` +
	`(SELECT CASE WHEN JSON_TYPE(JSON_EXTRACT(p.refs,'$.pr')) COLLATE utf8mb4_0900_bin='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(p.refs,'$.pr')) COLLATE utf8mb4_0900_bin END FROM tasks p WHERE p.id=t.parent_id AND p.title LIKE '[bundle %'), '')`

// srcEvLockedTasksMySQL returns the tasks whose ids idQuery selects, locked
// FOR UPDATE in ascending id order. PostgreSQL's ORDER BY ... FOR UPDATE
// locks in sort order; InnoDB locks rows in the order its access path visits
// them, which for a state or key index is not id order. So the ids are read
// first, without a lock, and the rows are then locked through one primary-key
// range scan, which is ascending. The callers hold the task-mutation lock
// (level 2), so no writer changes the set between the two statements.
func srcEvLockedTasksMySQL(ctx context.Context, tx transaction, idQuery string, args ...any) ([]Task, error) {
	rs, err := tx.Query(ctx, idQuery, args...)
	if err != nil {
		return nil, err
	}
	ids, err := collectRows(rs, rowTo[int64])
	if err != nil || len(ids) == 0 {
		return []Task{}, err
	}
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	rs, err = tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id IN (`+placeholders(len(ids))+`) ORDER BY id FOR UPDATE`, idArgs...)
	if err != nil {
		return nil, err
	}
	return collectRows(rs, func(row row) (Task, error) {
		var x Task
		return x, scanTask(row, &x)
	})
}

// srcEvChangesGate remains as a named seam for call sites shared with other
// unported tables. The changes tables are part of the MySQL baseline now.
func srcEvChangesGate(tx transaction) error {
	return nil
}

// srcEvRecordChangeTx is recordChangeTx for a source event. B5 dependency:
// recordChangeTx writes changes, change_tasks and reviews. It writes nothing
// for a PR URL parsePRURL cannot read, so only that case skips it on MySQL;
// every other event refuses before the call.
func srcEvRecordChangeTx(ctx context.Context, tx transaction, x Task, in TaskSourceEvent, at, now time.Time) error {
	if dialectFor(tx) == mysqlDialect {
		return recordChangeTxMySQL(ctx, tx, x, in, at, now)
	}
	return recordChangeTx(ctx, pgxTx(tx), x, in, at, now)
}

func recordChangeTxMySQL(ctx context.Context, tx transaction, x Task, in TaskSourceEvent, at, now time.Time) error {
	repo, n, ok := parsePRURL(in.PR)
	if !ok {
		return nil
	}
	state, head, base, mergeSHA := ChangeOpen, nullStr(in.HeadSHA), nullStr(in.BaseSHA), nullStr(in.MergeSHA)
	var mergedAt *time.Time
	shaFromMerge := false
	switch in.Source {
	case SourceMergewakeMerge:
		state, mergedAt, shaFromMerge = ChangeMerged, &at, true
	case SourceMergewakeRevert:
		state, head, base = ChangeMerged, nil, nil
	}
	upsert := func(repo string, n int, state string, head, base, mergeSHA *string, mergedAt *time.Time, shaFromMerge bool) (int64, error) {
		_, err := tx.Exec(ctx, `INSERT INTO changes(repo,pr_number,state,head_sha,base_sha,merge_sha,opened_at,merged_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE
	head_sha=IF(state='merged' AND ?=FALSE,head_sha,COALESCE(VALUES(head_sha),head_sha)),
	base_sha=IF(state='merged' AND ?=FALSE,base_sha,COALESCE(VALUES(base_sha),base_sha)),
	state=IF(VALUES(state)='merged','merged',state), merged_at=IF(VALUES(state)='merged',COALESCE(merged_at,VALUES(merged_at)),merged_at),
	merge_sha=COALESCE(VALUES(merge_sha),merge_sha),updated_at=VALUES(updated_at)`,
			repo, n, state, head, base, mergeSHA, at, mergedAt, now, now, shaFromMerge, shaFromMerge)
		if err != nil {
			return 0, err
		}
		var id int64
		err = tx.QueryRow(ctx, `SELECT id FROM changes WHERE repo=? AND pr_number=?`, repo, n).Scan(&id)
		return id, err
	}
	id, err := upsert(repo, n, state, head, base, mergeSHA, mergedAt, shaFromMerge)
	if err != nil {
		return err
	}
	if err = linkChangeTaskMySQL(ctx, tx, id, x.ID, ChangeImplements, false, now); err != nil {
		return err
	}
	if in.Source == SourceMergewakeReview {
		for _, rv := range in.Reviews {
			if err = insertReviewTxMySQL(ctx, tx, id, rv, now); err != nil {
				return err
			}
		}
	}
	if in.Source == SourceMergewakeRevert {
		revRepo, revN, ok := parsePRURL(in.RevertPR)
		if !ok {
			return nil
		}
		revID, e := upsert(revRepo, revN, ChangeMerged, nullStr(in.HeadSHA), nullStr(in.BaseSHA), nil, &at, false)
		if e != nil {
			return e
		}
		if revID != id {
			if e = linkChangeTaskMySQL(ctx, tx, revID, x.ID, ChangeReferences, true, now); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `UPDATE changes SET reverted_by_change_id=?,updated_at=? WHERE id=? AND reverted_by_change_id IS NULL`, revID, now, id); e != nil {
				return e
			}
		}
	}
	return nil
}

func linkChangeTaskMySQL(ctx context.Context, tx transaction, changeID, taskID int64, relation string, reclassify bool, now time.Time) error {
	query := `INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE task_id=task_id`
	if reclassify {
		query = `INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE relation=VALUES(relation)`
	}
	_, err := tx.Exec(ctx, query, changeID, taskID, relation, now)
	return err
}

func linkChangeTask(ctx context.Context, tx transaction, changeID, taskID int64, relation string, reclassify bool, now time.Time) error {
	if dialectFor(tx) == mysqlDialect {
		return linkChangeTaskMySQL(ctx, tx, changeID, taskID, relation, reclassify, now)
	}
	return linkChangeTaskTx(ctx, pgxTx(tx), changeID, taskID, relation, reclassify, now)
}

func insertReviewTxMySQL(ctx context.Context, tx transaction, changeID int64, rv TaskEventReview, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO reviews(change_id,reviewed_head,reviewed_base,patch_id,round,pass,lens,reviewer_job_id,family,profile,model,harness,effort,executor,fallback,policy_revision,contract_source,verdict,critical,high,medium,low,started_at,finished_at,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=id`,
		changeID, rv.ReviewedHead, nullStr(rv.ReviewedBase), nullStr(rv.PatchID), rv.Round, rv.Pass, rv.Lens, nullStr(rv.ReviewerJobID), nullStr(rv.Family), nullStr(rv.Profile), nullStr(rv.Model), nullStr(rv.Harness), nullStr(rv.Effort), nullStr(rv.Executor), rv.Fallback, nullStr(rv.PolicyRevision), nullStr(rv.ContractSource), nullStr(rv.Verdict), rv.Critical, rv.High, rv.Medium, rv.Low, rv.StartedAt, rv.FinishedAt, now, rv.Reviewer, rv.SameFamily, rv.Delta, rv.Quiet, rv.OutsideDiffBlocking, rv.PatchDigest)
	if err != nil {
		return err
	}
	var id int64
	var bare bool
	err = tx.QueryRow(ctx, `SELECT id,`+reviewBareMySQL+` FROM reviews WHERE change_id=? AND reviewed_head=? AND lens=? AND round=? AND pass=? FOR UPDATE`, changeID, rv.ReviewedHead, rv.Lens, rv.Round, rv.Pass).Scan(&id, &bare)
	if err != nil || !bare {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE reviews SET reviewed_base=?,patch_id=?,reviewer_job_id=?,family=?,profile=?,model=?,harness=?,effort=?,executor=?,fallback=?,policy_revision=?,contract_source=?,verdict=?,critical=?,high=?,medium=?,low=?,started_at=?,finished_at=?,
		reviewer=COALESCE(reviewer,?),same_family=COALESCE(same_family,?),delta=COALESCE(delta,?),quiet=COALESCE(quiet,?),outside_diff_blocking=COALESCE(outside_diff_blocking,?),patch_digest=COALESCE(patch_digest,?) WHERE id=? AND `+reviewBareMySQL,
		nullStr(rv.ReviewedBase), nullStr(rv.PatchID), nullStr(rv.ReviewerJobID), nullStr(rv.Family), nullStr(rv.Profile), nullStr(rv.Model), nullStr(rv.Harness), nullStr(rv.Effort), nullStr(rv.Executor), rv.Fallback, nullStr(rv.PolicyRevision), nullStr(rv.ContractSource), nullStr(rv.Verdict), rv.Critical, rv.High, rv.Medium, rv.Low, rv.StartedAt, rv.FinishedAt, rv.Reviewer, rv.SameFamily, rv.Delta, rv.Quiet, rv.OutsideDiffBlocking, rv.PatchDigest, id)
	return err
}

const reviewBareMySQL = `reviewed_base IS NULL AND patch_id IS NULL AND reviewer_job_id IS NULL AND family IS NULL AND profile IS NULL AND model IS NULL AND harness IS NULL AND effort IS NULL AND executor IS NULL AND fallback IS NULL AND policy_revision IS NULL AND contract_source IS NULL AND verdict IS NULL AND critical IS NULL AND high IS NULL AND medium IS NULL AND low IS NULL AND started_at IS NULL AND finished_at IS NULL`

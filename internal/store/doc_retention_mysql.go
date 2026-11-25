package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MySQL leg of document retention (hk 5057). Same predicates as
// doc_retention.go: review/mwr-* by merged change, screens/* by ended job,
// the merge link of doc_retention_merge.go when it is on, only unbound
// documents, audit row per deletion in the same transaction.
func (s *Store) mysqlPruneRetention(ctx context.Context, res DocRetentionResult, cutoff time.Time) (DocRetentionResult, error) {
	mwrLo, mwrHi := keyPrefixBounds(DocRetentionMwrPrefix)
	scrLo, scrHi := keyPrefixBounds(DocRetentionScreensPrefix)
	cut := cutoff.UTC()
	countEligible := `SELECT COUNT(*),COUNT(b.document_id) FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id=d.id WHERE d.` + "`key`" + `>=? AND d.` + "`key`" + `<? %s`
	var mwrMatched, mwrBound, scrMatched, scrBound int64
	if err := s.db().QueryRow(ctx, fmt.Sprintf(countEligible, mysqlReviewEligible), mwrLo, mwrHi, cut).Scan(&mwrMatched, &mwrBound); err != nil {
		return res, err
	}
	if err := s.db().QueryRow(ctx, fmt.Sprintf(countEligible, mysqlScreensEligible), scrLo, scrHi, cut).Scan(&scrMatched, &scrBound); err != nil {
		return res, err
	}
	res.Matched = mwrMatched + scrMatched
	res.SkippedBound = mwrBound + scrBound
	var mwrTotal, scrTotal int64
	if err := s.db().QueryRow(ctx, "SELECT COUNT(*) FROM documents d WHERE d.`key`>=? AND d.`key`<?", mwrLo, mwrHi).Scan(&mwrTotal); err != nil {
		return res, err
	}
	if err := s.db().QueryRow(ctx, "SELECT COUNT(*) FROM documents d WHERE d.`key`>=? AND d.`key`<?", scrLo, scrHi).Scan(&scrTotal); err != nil {
		return res, err
	}
	res.KeptActive = mwrTotal + scrTotal - res.Matched
	if !res.Applied {
		// Same selection order as the apply batches: review/mwr-* rows
		// first, then screens/* rows, capped at limit.
		drySizes := func(lo, hi, eligible string, want int) (int, error) {
			if want <= 0 {
				return 0, nil
			}
			rows, e := s.db().Query(ctx, fmt.Sprintf(`SELECT OCTET_LENGTH(d.body) FROM documents d WHERE d.`+"`key`"+`>=? AND d.`+"`key`"+`<? AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id=d.id) %s ORDER BY d.id LIMIT ?`, eligible), lo, hi, cut, want)
			if e != nil {
				return 0, e
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var size int64
				if e = rows.Scan(&size); e != nil {
					return 0, e
				}
				res.Bytes += size
				n++
			}
			return n, rows.Err()
		}
		nReview, e := drySizes(mwrLo, mwrHi, mysqlReviewEligible, res.Limit)
		if e != nil {
			return res, e
		}
		if _, e = drySizes(scrLo, scrHi, mysqlScreensEligible, res.Limit-nReview); e != nil {
			return res, e
		}
		return res, nil
	}
	for res.Deleted < int64(res.Limit) {
		var n int
		e := s.retryTx(ctx, func() error {
			var err error
			n, err = s.mysqlRetentionBatch(ctx, cut, min(DocRetentionBatch, res.Limit-int(res.Deleted)), res.PrunedBy, &res)
			return err
		})
		if e != nil {
			return res, e
		}
		if n == 0 {
			break
		}
	}
	return res, nil
}

// mysqlReviewEligible mirrors docRetentionReviewEligible: the merged change at
// or before the cutoff, linked only through the reviewer's job id. No task
// leg: ANY merged change on the document's task must not delete another
// change's prompt.
const mysqlReviewEligible = `AND EXISTS (SELECT 1 FROM reviews r JOIN changes c ON c.id=r.change_id WHERE r.reviewer_job_id=d.job AND d.job<>'' AND c.state='merged' AND c.merged_at IS NOT NULL AND c.merged_at<=?)`

const mysqlScreensEligible = `AND EXISTS (SELECT 1 FROM jobs j WHERE (j.job_id=d.job OR j.job_id=SUBSTRING_INDEX(SUBSTRING_INDEX(d.` + "`key`" + `,'/',2),'/',-1)) AND j.ended_at IS NOT NULL AND j.ended_at<=?)`

// mysqlRetentionBatch deletes up to batch retention-eligible documents
// (review first, then screens, then whole merge-link jobs while they fit when
// the leg is on) in one transaction with one audit row each, carrying the
// document's job and task_id.
func (s *Store) mysqlRetentionBatch(ctx context.Context, cutoff time.Time, batch int, prunedBy string, res *DocRetentionResult) (int, error) {
	// The merge leg's pick runs before the transaction: every statement in
	// it before the recheck is a locking read, so the recheck is its first
	// consistent read and its snapshot starts after the row locks are held.
	merge, e := s.mergeLinkBatchIDs(ctx, cutoff, batch)
	if e != nil {
		return 0, e
	}
	tx, release, e := s.begin(ctx)
	if e != nil {
		return 0, e
	}
	defer release()
	defer tx.Rollback(ctx)
	mwrLo, mwrHi := keyPrefixBounds(DocRetentionMwrPrefix)
	scrLo, scrHi := keyPrefixBounds(DocRetentionScreensPrefix)
	candidate := `SELECT d.id,d.` + "`key`" + `,d.sha256,OCTET_LENGTH(d.body),d.job,d.task_id FROM documents d WHERE d.` + "`key`" + `>=? AND d.` + "`key`" + `<? AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id=d.id) %s ORDER BY d.id LIMIT ?`
	var picked []retentionRow
	scan := func(rs rows) ([]retentionRow, error) {
		defer rs.Close()
		var out []retentionRow
		for rs.Next() {
			var r retentionRow
			if err := rs.Scan(&r.id, &r.key, &r.sha, &r.size, &r.job, &r.task); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rs.Err()
	}
	collect := func(lo, hi, eligible string, want int) error {
		if want <= 0 {
			return nil
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(candidate, eligible)+` FOR UPDATE`, lo, hi, cutoff, want)
		if err != nil {
			return err
		}
		got, err := scan(rows)
		picked = append(picked, got...)
		return err
	}
	if e = collect(mwrLo, mwrHi, mysqlReviewEligible, batch); e != nil {
		return 0, e
	}
	nReview := len(picked)
	if len(picked) < batch {
		if e = collect(scrLo, scrHi, mysqlScreensEligible, batch-len(picked)); e != nil {
			return 0, e
		}
	}
	nScreens := len(picked) - nReview
	var admittedJobs, skippedJobs []string
	if len(picked) < batch && len(merge.ids) > 0 {
		// The merge leg (hk 6280): lock the picked rows, then classify them
		// again under the lock, keep only the rows still eligible and admit
		// whole jobs while they fit (admitMergeJobs, hk 6290).
		skip := make(map[int64]bool, len(picked))
		for _, r := range picked {
			skip[r.id] = true
		}
		if ids := withoutIDs(merge.ids, skip); len(ids) > 0 {
			args := make([]any, len(ids))
			for i, id := range ids {
				args[i] = id
			}
			lock, err := tx.Query(ctx, "SELECT id FROM documents WHERE id IN ("+placeholders(len(ids))+") ORDER BY id FOR UPDATE", args...)
			if err != nil {
				return 0, err
			}
			for lock.Next() {
			}
			lock.Close()
			if err = lock.Err(); err != nil {
				return 0, err
			}
			q, qargs := mergeLinkRecheckSQL(true, cutoff, ids)
			rows, err := tx.Query(ctx, q, qargs...)
			if err != nil {
				return 0, err
			}
			still, err := scan(rows)
			if err != nil {
				return 0, err
			}
			var admitted []retentionRow
			admitted, admittedJobs, skippedJobs = admitMergeJobs(merge.jobs, still, batch-len(picked))
			picked = append(picked, admitted...)
		}
	}
	nMerge := len(picked) - nReview - nScreens
	if len(picked) == 0 {
		res.noteMergeSkips(nil, skippedJobs)
		return 0, nil
	}
	ids := make([]any, len(picked))
	keys := make([]any, len(picked))
	strKeys := make([]string, len(picked))
	var size int64
	now := time.Now().UTC()
	vals := make([]string, len(picked))
	logArgs := make([]any, 0, len(picked)*6)
	for i, r := range picked {
		ids[i], keys[i], strKeys[i] = r.id, r.key, r.key
		size += r.size
		vals[i] = "(?,?,?,?,?,?)"
		logArgs = append(logArgs, r.key, r.sha, now, prunedBy, r.job, r.task)
	}
	if _, e = tx.Exec(ctx, "DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id IN ("+placeholders(len(keys))+")", keys...); e != nil {
		return 0, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO doc_prune_log(document_key, sha256, pruned_at, pruned_by, job, task_id) VALUES "+strings.Join(vals, ","), logArgs...); e != nil {
		return 0, e
	}
	deleted, e := tx.Exec(ctx, "DELETE FROM documents WHERE id IN ("+placeholders(len(ids))+")", ids...)
	if e != nil {
		return 0, e
	}
	if e = deleteTaskDocRefsMySQLTx(ctx, tx, strKeys); e != nil {
		return 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return 0, e
	}
	if deleted != int64(len(ids)) {
		return 0, errors.New("retention prune deleted an unexpected row count")
	}
	res.Deleted += int64(len(ids))
	res.Bytes += size
	res.ReviewDeleted += int64(nReview)
	res.ScreensDeleted += int64(nScreens)
	res.MergeLinkDeleted += int64(nMerge)
	res.noteMergeSkips(admittedJobs, skippedJobs)
	return len(ids), nil
}

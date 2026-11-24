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
// only unbound documents, audit row per deletion in the same transaction.
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
// (review first, then screens) in one transaction with one audit row each.
func (s *Store) mysqlRetentionBatch(ctx context.Context, cutoff time.Time, batch int, prunedBy string, res *DocRetentionResult) (int, error) {
	tx, release, e := s.begin(ctx)
	if e != nil {
		return 0, e
	}
	defer release()
	defer tx.Rollback(ctx)
	mwrLo, mwrHi := keyPrefixBounds(DocRetentionMwrPrefix)
	scrLo, scrHi := keyPrefixBounds(DocRetentionScreensPrefix)
	candidate := `SELECT d.id,d.` + "`key`" + `,d.sha256,OCTET_LENGTH(d.body) FROM documents d WHERE d.` + "`key`" + `>=? AND d.` + "`key`" + `<? AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id=d.id) %s ORDER BY d.id LIMIT ?`
	var ids []any
	var keys []any
	var shas []string
	var strKeys []string
	var size int64
	nReview := 0
	collect := func(lo, hi, eligible string, want int) error {
		if want <= 0 {
			return nil
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(candidate, eligible)+` FOR UPDATE`, lo, hi, cutoff, want)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, n int64
			var key, sha string
			if err = rows.Scan(&id, &key, &sha, &n); err != nil {
				return err
			}
			ids = append(ids, id)
			keys = append(keys, key)
			shas = append(shas, sha)
			strKeys = append(strKeys, key)
			size += n
		}
		return rows.Err()
	}
	if e = collect(mwrLo, mwrHi, mysqlReviewEligible, batch); e != nil {
		return 0, e
	}
	nReview = len(ids)
	if len(ids) < batch {
		if e = collect(scrLo, scrHi, mysqlScreensEligible, batch-len(ids)); e != nil {
			return 0, e
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if _, e = tx.Exec(ctx, "DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id IN ("+placeholders(len(keys))+")", keys...); e != nil {
		return 0, e
	}
	now := time.Now().UTC()
	vals := make([]string, len(ids))
	args := make([]any, 0, len(ids)*4)
	for i := range ids {
		vals[i] = "(?,?,?,?)"
		args = append(args, strKeys[i], shas[i], now, prunedBy)
	}
	if _, e = tx.Exec(ctx, "INSERT INTO doc_prune_log(document_key, sha256, pruned_at, pruned_by) VALUES "+strings.Join(vals, ","), args...); e != nil {
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
	res.ScreensDeleted += int64(len(ids)) - int64(nReview)
	return len(ids), nil
}

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Document retention (hk 5057, hk 4899 A3, operator answer dr-5057-2 A).
//
// Which documents qualify: review/mwr-* whose change merged and screens/*
// whose job ended, both at least DocRetentionDays ago, plus (when
// DocRetentionMergeLinkApply is on) review/mwr-*, briefs/mwr-* and
// screens/mwr-* whose review job's change merged that long ago through the
// merge link (doc_retention_merge.go, hk 6280). reports/mwr-* is never
// deleted. 'Merged or ended' is
// read from the change row (state='merged' and merged_at) for review/mwr-*
// through the reviewer's job id (documents.job = reviews.reviewer_job_id),
// and from the job row (ended_at) for screens/* through documents.job or the
// key's second segment (screens/<job>/...). A document with no linked merged
// change or ended job is kept: only positive evidence of merge/end plus age
// deletes.
//
// Nothing else is deleted: only the prefixes above, and never a document
// a yt_article_bindings row still points to. attachment_refs rows filed
// against a deleted document (ref_kind='document') are removed with it, so no
// dangling ref survives; the attachment blobs stay.
//
// A deletion is recorded in doc_prune_log (who, what, when, a sha list, and
// the document's job and task_id) in the same transaction that deletes the
// document. Dry runs write no log row. The job and task_id are the merge
// link's evidence after the delete (hk 6289): a deleted document still counts
// as a witness of its job's task, and a deleted second task still makes the
// job ambiguous.
//
// It is idempotent (a second run finds no row to delete and logs nothing)
// and bounded per run (limit, in DocRetentionBatch-sized transactions).
//
// The existing doc_prune (review/mwr-* by updated_at, hk 3639) is the base:
// this extends it with a second pruner for retention rather than replacing
// it, so the old prefix/older-than path keeps working unchanged.
const (
	// DocRetentionMwrPrefix holds mergewake reviewer prompts.
	DocRetentionMwrPrefix = "review/mwr-"
	// DocRetentionScreensPrefix holds pane screens filed per job.
	DocRetentionScreensPrefix = "screens/"
	// DocRetentionDays is the age after merge/end a body is deleted.
	DocRetentionDays = 30
)

// DocRetentionBatch bounds one transaction; DocRetentionMaxLimit bounds one run.
const (
	DocRetentionBatch    = DocPruneBatch
	DocRetentionMaxLimit = DocPruneMaxLimit
)

// DocRetentionResult counts one retention run. Matched, SkippedBound,
// KeptActive, the dry-run Bytes, ReviewDeleted and ScreensDeleted describe the
// hk 5057 legs (review/mwr-* by reviewer job, screens/* by job end) exactly as
// before the merge link: Matched is every document those legs make eligible
// including bound ones; SkippedBound of them have a YouTrack article binding
// and are kept. KeptActive is every document under review/mwr- or screens/
// that those legs do not make eligible. Deleted is zero on a dry run, where
// Bytes is the body size those legs would delete, capped by limit.
//
// The merge link (hk 6280) is reported apart: MergeLinkApply says whether an
// apply run deletes by it, MergeLink is its per-family classification (dry run
// only, whether the switch is on or off), and MergeLinkDeleted counts the
// documents an apply run deleted by it alone. On apply, Deleted and Bytes
// include those documents. The merge leg deletes whole jobs only, and only
// while all of a job's eligible documents fit the batch and the run's limit
// (hk 6290): MergeLinkPickSkippedJobs counts the distinct jobs the merge
// leg's capped pick reached and left whole for lack of room in some batch of
// this run and never admitted later in it. It is not every eligible job left
// at run end: a job the pick never reached is not counted. A limit smaller
// than every eligible job deletes nothing by the merge link and says why.
type DocRetentionResult struct {
	OlderThan      time.Time `json:"older_than"`
	Limit          int       `json:"limit"`
	Applied        bool      `json:"applied"`
	PrunedBy       string    `json:"pruned_by"`
	Matched        int64     `json:"matched"`
	Deleted        int64     `json:"deleted"`
	SkippedBound   int64     `json:"skipped_bound"`
	KeptActive     int64     `json:"kept_active"`
	Bytes          int64     `json:"bytes"`
	ReviewDeleted  int64     `json:"review_deleted"`
	ScreensDeleted int64     `json:"screens_deleted"`

	MergeLinkApply   bool                          `json:"merge_link_apply"`
	MergeLinkDeleted int64                         `json:"merge_link_deleted"`
	MergeLink        map[string]DocRetentionFamily `json:"merge_link,omitempty"`

	MergeLinkPickSkippedJobs int64 `json:"merge_link_pick_skipped_jobs"`
	// mergeSkipped is the set behind MergeLinkPickSkippedJobs (noteMergeSkips).
	mergeSkipped map[string]bool
}

// docRetentionReviewEligible is the merge predicate for a documents row
// aliased d with the cutoff in $3: a merged change at or before the cutoff,
// linked through the reviewer's job id (documents.job =
// reviews.reviewer_job_id). Only the job leg is used: a task leg (ANY change
// linked to d.task_id merged) would delete an open PR's prompt when its task
// also implements an older merged PR, so it is kept, never deleted, without
// its own merged change.
const docRetentionReviewEligible = `AND EXISTS (SELECT 1 FROM reviews r JOIN changes c ON c.id = r.change_id
  WHERE r.reviewer_job_id = d.job AND d.job <> ''
  AND c.state = 'merged' AND c.merged_at IS NOT NULL AND c.merged_at <= $3)`

// docRetentionScreensEligible is the job-end predicate for a documents row
// aliased d with the cutoff in $3: an ended job at or before the cutoff,
// linked through documents.job or the key's second segment.
const docRetentionScreensEligible = `AND EXISTS (SELECT 1 FROM jobs j
  WHERE (j.job_id = d.job OR j.job_id = split_part(d.key, '/', 2))
  AND j.ended_at IS NOT NULL AND j.ended_at <= $3)`

// docRetentionCandidates lists unbound retention-eligible documents under one
// prefix, oldest id first, so each batch is deterministic.
const docRetentionCandidatesPG = `SELECT d.id, d.key, d.sha256, octet_length(d.body), d.job, d.task_id FROM documents d
WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2
AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id = d.id)
%s
ORDER BY d.id LIMIT $4`

// PruneRetention deletes retention-eligible review/mwr-* and screens/*
// documents whose change merged or whose job ended at or before cutoff. It
// is a dry run unless apply is true. prunedBy names who runs it (the API
// client id or "retention") and is recorded on every deleted document; it is
// required even for a dry run so the caller is always identified.
func (s *Store) PruneRetention(ctx context.Context, cutoff time.Time, limit int, apply bool, prunedBy string) (DocRetentionResult, error) {
	res := DocRetentionResult{OlderThan: cutoff.UTC(), Limit: limit, Applied: apply, PrunedBy: prunedBy, MergeLinkApply: s.mergeLinkApplyOn()}
	if cutoff.IsZero() || cutoff.After(time.Now()) {
		return res, fmt.Errorf("%w: older_than must be a time in the past", ErrDocPruneInvalid)
	}
	if limit < 1 || limit > DocRetentionMaxLimit {
		return res, fmt.Errorf("%w: limit must be 1 to %d", ErrDocPruneInvalid, DocRetentionMaxLimit)
	}
	if prunedBy == "" {
		return res, fmt.Errorf("%w: pruned_by is required", ErrDocPruneInvalid)
	}
	if !apply {
		ml, e := s.mergeLinkCounts(ctx, cutoff)
		if e != nil {
			return res, e
		}
		res.MergeLink = ml
	}
	if s.dialect == mysqlDialect {
		return s.mysqlPruneRetention(ctx, res, cutoff)
	}
	mwrLo, mwrHi := keyPrefixBounds(DocRetentionMwrPrefix)
	scrLo, scrHi := keyPrefixBounds(DocRetentionScreensPrefix)
	countEligible := `SELECT count(*), count(b.document_id) FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id = d.id WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2 %s`
	var mwrMatched, mwrBound, scrMatched, scrBound int64
	if e := s.pool.QueryRow(ctx, fmt.Sprintf(countEligible, docRetentionReviewEligible), mwrLo, mwrHi, cutoff.UTC()).Scan(&mwrMatched, &mwrBound); e != nil {
		return res, e
	}
	if e := s.pool.QueryRow(ctx, fmt.Sprintf(countEligible, docRetentionScreensEligible), scrLo, scrHi, cutoff.UTC()).Scan(&scrMatched, &scrBound); e != nil {
		return res, e
	}
	res.Matched = mwrMatched + scrMatched
	res.SkippedBound = mwrBound + scrBound
	var mwrTotal, scrTotal int64
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM documents d WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2`, mwrLo, mwrHi).Scan(&mwrTotal); e != nil {
		return res, e
	}
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM documents d WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2`, scrLo, scrHi).Scan(&scrTotal); e != nil {
		return res, e
	}
	res.KeptActive = mwrTotal + scrTotal - res.Matched
	if !apply {
		// Bytes is the body size apply would delete: review/mwr-* rows first
		// in id order, then screens/* rows, capped at limit — the same
		// selection order as pruneRetentionBatch, so the preview and the
		// apply agree on the row set.
		drySizes := func(lo, hi, eligible string, want int) (int, error) {
			if want <= 0 {
				return 0, nil
			}
			rows, e := s.pool.Query(ctx, fmt.Sprintf(`SELECT octet_length(d.body) FROM documents d
WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2
AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id = d.id)
%s
ORDER BY d.id LIMIT $4`, eligible), lo, hi, cutoff.UTC(), want)
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
		nReview, e := drySizes(mwrLo, mwrHi, docRetentionReviewEligible, limit)
		if e != nil {
			return res, e
		}
		if _, e = drySizes(scrLo, scrHi, docRetentionScreensEligible, limit-nReview); e != nil {
			return res, e
		}
		return res, nil
	}
	for res.Deleted < int64(limit) {
		batch := min(DocRetentionBatch, limit-int(res.Deleted))
		n, e := s.pruneRetentionBatch(ctx, cutoff.UTC(), batch, prunedBy, &res)
		if e != nil {
			return res, e
		}
		if n == 0 {
			break
		}
	}
	return res, nil
}

// pruneRetentionBatch deletes up to batch retention-eligible documents (review
// first, then screens, then the merge link when it is on) in one transaction, records each deletion in
// doc_prune_log, and adds the counts to res. It returns how many documents it
// deleted; zero means the candidates are exhausted.
func (s *Store) pruneRetentionBatch(ctx context.Context, cutoff time.Time, batch int, prunedBy string, res *DocRetentionResult) (int, error) {
	var n int
	err := s.retryTx(ctx, func() error {
		var err error
		n, err = s.pruneRetentionBatchOnce(ctx, cutoff, batch, prunedBy, res)
		return err
	})
	return n, err
}

// pruneRetentionBatchOnce is one whole retention batch (retried on MySQL after a
// deadlock or lock-wait timeout). Its deletes are database rows only and roll
// back with the transaction, and res changes only after the commit, so a retry
// re-selects the same candidates and counts them once.
func (s *Store) pruneRetentionBatchOnce(ctx context.Context, cutoff time.Time, batch int, prunedBy string, res *DocRetentionResult) (int, error) {
	merge, e := s.mergeLinkBatchIDs(ctx, cutoff, batch)
	if e != nil {
		return 0, e
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	mwrLo, mwrHi := keyPrefixBounds(DocRetentionMwrPrefix)
	scrLo, scrHi := keyPrefixBounds(DocRetentionScreensPrefix)
	var picked []retentionRow
	scan := func(rows pgx.Rows) ([]retentionRow, error) {
		defer rows.Close()
		var out []retentionRow
		for rows.Next() {
			var r retentionRow
			if e := rows.Scan(&r.id, &r.key, &r.sha, &r.size, &r.job, &r.task); e != nil {
				return nil, e
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	collect := func(lo, hi, eligible string, want int) error {
		if want <= 0 {
			return nil
		}
		rows, e := tx.Query(ctx, fmt.Sprintf(docRetentionCandidatesPG+` FOR UPDATE OF d`, eligible), lo, hi, cutoff, want)
		if e != nil {
			return e
		}
		got, e := scan(rows)
		picked = append(picked, got...)
		return e
	}
	if e = collect(mwrLo, mwrHi, docRetentionReviewEligible, batch); e != nil {
		return 0, e
	}
	nReview := len(picked)
	if len(picked) < batch {
		if e = collect(scrLo, scrHi, docRetentionScreensEligible, batch-len(picked)); e != nil {
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
			if _, e = tx.Exec(ctx, `SELECT id FROM documents WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids); e != nil {
				return 0, e
			}
			q, args := mergeLinkRecheckSQL(false, cutoff, ids)
			rows, e := tx.Query(ctx, q, args...)
			if e != nil {
				return 0, e
			}
			still, e := scan(rows)
			if e != nil {
				return 0, e
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
	ids := make([]int64, len(picked))
	keys := make([]string, len(picked))
	shas := make([]string, len(picked))
	jobs := make([]string, len(picked))
	tasks := make([]*int64, len(picked))
	var bytes int64
	for i, r := range picked {
		ids[i], keys[i], shas[i], jobs[i], tasks[i] = r.id, r.key, r.sha, r.job, r.task
		bytes += r.size
	}
	if _, e = tx.Exec(ctx, `DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id = ANY($1)`, keys); e != nil {
		return 0, e
	}
	now := time.Now().UTC()
	if _, e = tx.Exec(ctx, `INSERT INTO doc_prune_log(document_key, sha256, pruned_at, pruned_by, job, task_id)
SELECT k, h, $3, $4, j, t FROM unnest($1::text[], $2::text[], $5::text[], $6::bigint[]) AS u(k, h, j, t)`, keys, shas, now, prunedBy, jobs, tasks); e != nil {
		return 0, e
	}
	tag, e := tx.Exec(ctx, `DELETE FROM documents WHERE id = ANY($1)`, ids)
	if e != nil {
		return 0, e
	}
	if _, e = tx.Exec(ctx, `DELETE FROM task_doc_refs WHERE doc_key = ANY($1)`, keys); e != nil {
		return 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return 0, e
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return 0, errors.New("retention prune deleted an unexpected row count")
	}
	res.Deleted += int64(len(ids))
	res.Bytes += bytes
	res.ReviewDeleted += int64(nReview)
	res.ScreensDeleted += int64(nScreens)
	res.MergeLinkDeleted += int64(nMerge)
	res.noteMergeSkips(admittedJobs, skippedJobs)
	return len(ids), nil
}

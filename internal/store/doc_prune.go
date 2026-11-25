package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DocPrunePrefix is the one key prefix doc prune may delete: mergewake's
// review/mwr-* reviewer briefs (hk 3639). Every other prefix is refused, so a
// review/<date>/... document can never match.
const DocPrunePrefix = "review/mwr-"

// DocPruneMaxLimit bounds the documents one run may delete. DocPruneBatch is
// how many one transaction deletes, so a long run never holds one large lock.
const (
	DocPruneMaxLimit = 100000
	DocPruneBatch    = 500
)

// ErrDocPruneInvalid refuses a prune request the rules above do not allow.
// The text after "invalid_doc_prune: " names the rule that failed.
var ErrDocPruneInvalid = errors.New("invalid_doc_prune")

// DocPruneResult counts one run. Matched is every document under the prefix
// and older than the cutoff; SkippedBound of them have a YouTrack article
// binding and are kept. Deleted is zero on a dry run, where Bytes is the body
// size the run would delete, capped by limit.
type DocPruneResult struct {
	Prefix       string    `json:"prefix"`
	OlderThan    time.Time `json:"older_than"`
	Limit        int       `json:"limit"`
	Applied      bool      `json:"applied"`
	Matched      int64     `json:"matched"`
	Deleted      int64     `json:"deleted"`
	SkippedBound int64     `json:"skipped_bound"`
	Bytes        int64     `json:"bytes"`
}

// docPruneCandidates lists unbound documents under the prefix older than the
// cutoff, oldest key-order first by id, so each batch is deterministic. The
// prefix is the byte-wise key range $1 <= key < $2 (documents_key_c).
const docPruneCandidates = `SELECT d.id, d.key, octet_length(d.body), d.sha256, d.job, d.task_id FROM documents d
WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2 AND d.updated_at < $3
AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id = d.id)
ORDER BY d.id LIMIT $4`

// PruneDocuments deletes review/mwr-* documents last updated before olderThan.
// It is a dry run unless apply is true: a dry run deletes nothing and reports
// what would go. A document with a YouTrack article binding is kept and
// counted in SkippedBound. Each batch deletes the document rows and their
// document attachment refs in one transaction; the sync trigger logs the
// deletes, and each deleted document leaves one doc_prune_log row (prunedBy,
// key, sha256, job and task_id) in the same transaction, so the merge link
// (doc_retention_merge.go, hk 6289) still sees the task evidence of a
// review/mwr-* it deleted. The cutoff comes from the caller's clock, so the
// table uses no database now(). prunedBy names who runs it; it is required
// even for a dry run, as in PruneRetention.
func (s *Store) PruneDocuments(ctx context.Context, prefix string, olderThan time.Time, limit int, apply bool, prunedBy string) (DocPruneResult, error) {
	res := DocPruneResult{Prefix: prefix, OlderThan: olderThan.UTC(), Limit: limit, Applied: apply}
	if prefix != DocPrunePrefix {
		return res, fmt.Errorf("%w: prefix must be %s", ErrDocPruneInvalid, DocPrunePrefix)
	}
	if olderThan.IsZero() || olderThan.After(time.Now()) {
		return res, fmt.Errorf("%w: older_than must be a time in the past", ErrDocPruneInvalid)
	}
	if limit < 1 || limit > DocPruneMaxLimit {
		return res, fmt.Errorf("%w: limit must be 1 to %d", ErrDocPruneInvalid, DocPruneMaxLimit)
	}
	if prunedBy == "" {
		return res, fmt.Errorf("%w: pruned_by is required", ErrDocPruneInvalid)
	}
	if s.dialect == mysqlDialect {
		return s.mysqlPruneDocuments(ctx, res, olderThan, prunedBy)
	}
	lo, hi := keyPrefixBounds(prefix)
	if e := s.pool.QueryRow(ctx, `SELECT count(*), count(b.document_id) FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id = d.id WHERE d.key COLLATE "C" >= $1 AND d.key COLLATE "C" < $2 AND d.updated_at < $3`, lo, hi, olderThan.UTC()).Scan(&res.Matched, &res.SkippedBound); e != nil {
		return res, e
	}
	if !apply {
		rows, e := s.pool.Query(ctx, docPruneCandidates, lo, hi, olderThan.UTC(), limit)
		if e != nil {
			return res, e
		}
		defer rows.Close()
		for rows.Next() {
			var r retentionRow
			if e = rows.Scan(&r.id, &r.key, &r.size, &r.sha, &r.job, &r.task); e != nil {
				return res, e
			}
			res.Bytes += r.size
		}
		return res, rows.Err()
	}
	for res.Deleted < int64(limit) {
		batch := min(DocPruneBatch, limit-int(res.Deleted))
		n, e := s.pruneDocumentBatch(ctx, prefix, olderThan.UTC(), batch, prunedBy, &res)
		if e != nil {
			return res, e
		}
		if n == 0 {
			break
		}
	}
	return res, nil
}

// pruneDocumentBatch deletes up to batch candidates in one transaction and
// adds the deleted count and body bytes to res. It returns how many documents
// it found; zero means the candidates are exhausted.
func (s *Store) pruneDocumentBatch(ctx context.Context, prefix string, olderThan time.Time, batch int, prunedBy string, res *DocPruneResult) (int, error) {
	var out int
	rerr := s.retryTx(ctx, func() error {
		tx, e := s.pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		lo, hi := keyPrefixBounds(prefix)
		rows, e := tx.Query(ctx, docPruneCandidates+` FOR UPDATE OF d`, lo, hi, olderThan, batch)
		if e != nil {
			return e
		}
		var ids []int64
		var keys, shas, jobs []string
		var tasks []*int64
		var bytes int64
		for rows.Next() {
			var r retentionRow
			if e = rows.Scan(&r.id, &r.key, &r.size, &r.sha, &r.job, &r.task); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, r.id)
			keys = append(keys, r.key)
			shas = append(shas, r.sha)
			jobs = append(jobs, r.job)
			tasks = append(tasks, r.task)
			bytes += r.size
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		if len(ids) == 0 {
			return nil
		}
		if _, e = tx.Exec(ctx, `DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id = ANY($1)`, keys); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO doc_prune_log(document_key, sha256, pruned_at, pruned_by, job, task_id)
SELECT k, h, $3, $4, j, t FROM unnest($1::text[], $2::text[], $5::text[], $6::bigint[]) AS u(k, h, j, t)`, keys, shas, time.Now().UTC(), prunedBy, jobs, tasks); e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `DELETE FROM documents WHERE id = ANY($1)`, ids)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `DELETE FROM task_doc_refs WHERE doc_key = ANY($1)`, keys); e != nil {
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
		res.Deleted += tag.RowsAffected()
		res.Bytes += bytes
		out = len(ids)
		return nil
	})
	return out, rerr
}

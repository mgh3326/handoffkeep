package store

import (
	"context"
	"time"
)

// mysqlPruneDocuments deletes documents that no article binding and no
// attachment reference holds. Both tables exist on MySQL now (yt_article_bindings
// arrived with B4b, hk 5077), so B3's refusal is gone: the binding guard is
// always part of the delete. What remains for the integration pass (hk 5088)
// is the cross-area check that every MySQL caller of the prune sees the same
// SkippedBound count as PostgreSQL.
func (s *Store) mysqlPruneDocuments(ctx context.Context, res DocPruneResult, older time.Time) (DocPruneResult, error) {
	lo, hi := keyPrefixBounds(res.Prefix)
	count := "SELECT COUNT(*),COUNT(b.document_id) FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id=d.id WHERE d.`key`>=? AND d.`key`<? AND d.updated_at<?"
	if err := s.db().QueryRow(ctx, count, lo, hi, older.UTC()).Scan(&res.Matched, &res.SkippedBound); err != nil {
		return res, err
	}
	candidate := "SELECT d.id,d.`key`,OCTET_LENGTH(d.body) FROM documents d WHERE d.`key`>=? AND d.`key`<? AND d.updated_at<?"
	candidate += " AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id=d.id)"
	candidate += " ORDER BY d.id LIMIT ?"
	if !res.Applied {
		rows, e := s.db().Query(ctx, candidate, lo, hi, older.UTC(), res.Limit)
		if e != nil {
			return res, e
		}
		defer rows.Close()
		for rows.Next() {
			var id, size int64
			var key string
			if e = rows.Scan(&id, &key, &size); e != nil {
				return res, e
			}
			res.Bytes += size
		}
		return res, rows.Err()
	}
	for res.Deleted < int64(res.Limit) {
		var n int
		e := s.retryTx(ctx, func() error {
			var err error
			n, err = func() (int, error) {
				tx, release, e := s.begin(ctx)
				if e != nil {
					return 0, e
				}
				defer release()
				defer tx.Rollback(ctx)
				rows, e := tx.Query(ctx, candidate+" FOR UPDATE", lo, hi, older.UTC(), min(DocPruneBatch, res.Limit-int(res.Deleted)))
				if e != nil {
					return 0, e
				}
				var ids, keys []any
				var docKeys []string
				var size int64
				for rows.Next() {
					var id, n int64
					var key string
					if e = rows.Scan(&id, &key, &n); e != nil {
						rows.Close()
						return 0, e
					}
					ids = append(ids, id)
					keys = append(keys, key)
					docKeys = append(docKeys, key)
					size += n
				}
				e = rows.Err()
				rows.Close()
				if e != nil {
					return 0, e
				}
				if len(ids) == 0 {
					return 0, nil
				}
				if _, e = tx.Exec(ctx, "DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id IN ("+placeholders(len(keys))+")", keys...); e != nil {
					return 0, e
				}
				deleted, e := tx.Exec(ctx, "DELETE FROM documents WHERE id IN ("+placeholders(len(ids))+")", ids...)
				if e != nil {
					return 0, e
				}
				if e = deleteTaskDocRefsMySQLTx(ctx, tx, docKeys); e != nil {
					return 0, e
				}
				if e = tx.Commit(ctx); e != nil {
					return 0, e
				}
				res.Deleted += deleted
				res.Bytes += size
				return len(ids), nil
			}()
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

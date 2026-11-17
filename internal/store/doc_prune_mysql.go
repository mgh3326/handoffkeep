package store

import (
	"context"
	"time"
)

// mysqlPruneDocuments requires the binding and attachment tables before it
// can delete: another binary may add them while this process is running.
func (s *Store) mysqlPruneDocuments(ctx context.Context, res DocPruneResult, older time.Time) (DocPruneResult, error) {
	bindings, err := s.mysqlTableExists(ctx, s.db(), "yt_article_bindings")
	if err != nil {
		return res, err
	}
	refs, err := s.mysqlTableExists(ctx, s.db(), "attachment_refs")
	if err != nil {
		return res, err
	}
	if !bindings || !refs {
		return res, ErrDialectUnsupported
	}
	lo, hi := keyPrefixBounds(res.Prefix)
	count := "SELECT COUNT(*),0 FROM documents d WHERE d.`key`>=? AND d.`key`<? AND d.updated_at<?"
	if bindings {
		count = "SELECT COUNT(*),COUNT(b.document_id) FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id=d.id WHERE d.`key`>=? AND d.`key`<? AND d.updated_at<?"
	}
	if err = s.db().QueryRow(ctx, count, lo, hi, older.UTC()).Scan(&res.Matched, &res.SkippedBound); err != nil {
		return res, err
	}
	candidate := "SELECT d.id,d.`key`,OCTET_LENGTH(d.body) FROM documents d WHERE d.`key`>=? AND d.`key`<? AND d.updated_at<?"
	if bindings {
		candidate += " AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b WHERE b.document_id=d.id)"
	}
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
		tx, release, e := s.begin(ctx)
		if e != nil {
			return res, e
		}
		n, e := func() (int, error) {
			defer release()
			defer tx.Rollback(ctx)
			rows, e := tx.Query(ctx, candidate+" FOR UPDATE", lo, hi, older.UTC(), min(DocPruneBatch, res.Limit-int(res.Deleted)))
			if e != nil {
				return 0, e
			}
			var ids, keys []any
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
			if refs {
				if _, e = tx.Exec(ctx, "DELETE FROM attachment_refs WHERE ref_kind='document' AND ref_id IN ("+placeholders(len(keys))+")", keys...); e != nil {
					return 0, e
				}
			}
			deleted, e := tx.Exec(ctx, "DELETE FROM documents WHERE id IN ("+placeholders(len(ids))+")", ids...)
			if e != nil {
				return 0, e
			}
			if e = tx.Commit(ctx); e != nil {
				return 0, e
			}
			res.Deleted += deleted
			res.Bytes += size
			return len(ids), nil
		}()
		if e != nil {
			return res, e
		}
		if n == 0 {
			break
		}
	}
	return res, nil
}

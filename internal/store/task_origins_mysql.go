package store

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// addTaskOriginMySQL is AddTaskOrigin's MySQL path. The PostgreSQL statement
// is one upsert whose RETURNING and xmax say whether the row is new; MySQL has
// neither, and ON DUPLICATE KEY UPDATE answers "inserted" and "found but
// unchanged" with the same affected-row count under ClientFoundRows. So the
// origin's identity is serialized with an hk_locks row (the first and only lock
// of the transaction, a leaf like PostgreSQL's KEY SHARE on one task row, level
// 4 of lockorder.go) and the three outcomes are read explicitly: no row (insert
// it, or the task does not exist), a row that differs (refresh it), a row that
// does not (answer it unchanged).
//
// No ON DUPLICATE KEY is used, so the table's other unique keys cannot answer
// for the identity: task_origins_youtrack_task and task_origins_youtrack_external
// are NULL for every provider AddTaskOrigin accepts (linear, github, sentry).
func (s *Store) addTaskOriginMySQL(ctx context.Context, taskID int64, by string, in TaskOriginInput, now time.Time) (TaskOrigin, bool, error) {
	var x TaskOrigin
	var created bool
	err := s.retryTx(ctx, func() error {
		x, created = TaskOrigin{}, false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		identity := in.Provider + "\x00" + in.Namespace + "\x00" + in.ExternalID + "\x00" + strconv.FormatInt(taskID, 10)
		if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("task_origin", identity), false); err != nil {
			return err
		}
		const byIdentity = `SELECT ` + taskOriginColumns + ` FROM task_origins WHERE provider=? AND namespace=? AND external_id=? AND task_id=?`
		err = scanTaskOrigin(tx.QueryRow(ctx, byIdentity+` FOR UPDATE`, in.Provider, in.Namespace, in.ExternalID, taskID), &x)
		switch {
		case errors.Is(err, ErrNoRows):
			n, err := tx.Exec(ctx, `INSERT INTO task_origins(task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at)
SELECT t.id,?,?,?,?,?,?,?,?,? FROM tasks t WHERE t.id=?`,
				in.Provider, in.Namespace, in.ExternalID, nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), nullIfEmpty(in.Title), now, by, now, taskID)
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrTaskNotFound
			}
			created = true
			if err = scanTaskOrigin(tx.QueryRow(ctx, byIdentity, in.Provider, in.Namespace, in.ExternalID, taskID), &x); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// The refresh runs only when a given value differs, as in the
			// PostgreSQL WHERE; a value left out keeps the stored one.
			n, err := tx.Exec(ctx, `UPDATE task_origins SET display_ref=COALESCE(?,display_ref),url=COALESCE(?,url),title_snapshot=COALESCE(?,title_snapshot),snapshot_at=?
WHERE provider=? AND namespace=? AND external_id=? AND task_id=?
AND NOT (COALESCE(?,display_ref)<=>display_ref AND COALESCE(?,url)<=>url AND COALESCE(?,title_snapshot)<=>title_snapshot)`,
				nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), nullIfEmpty(in.Title), now,
				in.Provider, in.Namespace, in.ExternalID, taskID,
				nullIfEmpty(in.DisplayRef), nullIfEmpty(in.URL), nullIfEmpty(in.Title))
			if err != nil {
				return err
			}
			if n > 0 {
				if err = scanTaskOrigin(tx.QueryRow(ctx, byIdentity, in.Provider, in.Namespace, in.ExternalID, taskID), &x); err != nil {
					return err
				}
			}
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return TaskOrigin{}, false, err
	}
	return x, created, nil
}

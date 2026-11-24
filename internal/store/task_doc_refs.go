package store

import (
	"context"
	"errors"
	"time"
)

// ErrDocNotFound refuses a reference to a document key that is not stored. Its
// text is the wire code (TestErrorRuleCodesMatchSentinels).
var ErrDocNotFound = errors.New("doc_not_found")

// maxDotsRequestDocs bounds the references one dots request may carry.
const maxDotsRequestDocs = 20

// TaskDocRef is a reference from a task to a document it does not own. The
// binding (documents.task_id) stays the one owner link; a reference lets many
// tasks cite one living document, such as a schema document that several
// verify requests check. A reference whose document has been removed is not
// listed.
type TaskDocRef struct {
	TaskID    int64     `json:"task_id"`
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// AddTaskDocRef records a reference from task taskID to the document key. It
// reports changed=false when the reference already exists. A key with no stored
// document is refused with ErrDocNotFound, and an unknown task with
// ErrTaskNotFound.
func (s *Store) AddTaskDocRef(ctx context.Context, taskID int64, key, by string) (bool, error) {
	if taskID < 1 || !ValidDocKey(key) || !ValidActor(by) {
		return false, errors.New("invalid task doc ref")
	}
	var changed bool
	err := s.retryTx(ctx, func() error {
		changed = false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = requireTaskRowTx(ctx, tx, taskID); err != nil {
			return err
		}
		changed, err = addTaskDocRefTx(ctx, tx, taskID, key, by, time.Now().UTC())
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	return changed, err
}

// RemoveTaskDocRef drops the reference from task taskID to the document key.
// It reports changed=false when no such reference exists.
func (s *Store) RemoveTaskDocRef(ctx context.Context, taskID int64, key string) (bool, error) {
	if taskID < 1 || !ValidDocKey(key) {
		return false, errors.New("invalid task doc ref")
	}
	var changed bool
	err := s.retryTx(ctx, func() error {
		changed = false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = requireTaskRowTx(ctx, tx, taskID); err != nil {
			return err
		}
		n, err := tx.Exec(ctx, sqlFor(tx, `DELETE FROM task_doc_refs WHERE task_id=$1 AND doc_key=$2`, "DELETE FROM task_doc_refs WHERE task_id=? AND doc_key=?"), taskID, key)
		if err != nil {
			return err
		}
		changed = n > 0
		return tx.Commit(ctx)
	})
	return changed, err
}

// ListTaskDocRefs lists the references of task taskID in document key order.
// Each names a document that is stored now; a reference left behind by a
// removed document is not returned.
func (s *Store) ListTaskDocRefs(ctx context.Context, taskID int64) ([]TaskDocRef, error) {
	if taskID < 1 {
		return nil, errors.New("invalid task doc ref")
	}
	var n int64
	if err := s.db().QueryRow(ctx, s.q(`SELECT COUNT(*) FROM tasks WHERE id=$1`, "SELECT COUNT(*) FROM tasks WHERE id=?"), taskID).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrTaskNotFound
	}
	rows, err := s.db().Query(ctx, s.q(
		`SELECT r.task_id,r.doc_key,d.kind,r.created_at,r.created_by FROM task_doc_refs r JOIN documents d ON d.key=r.doc_key WHERE r.task_id=$1 ORDER BY r.doc_key`,
		"SELECT r.task_id,r.doc_key,d.kind,r.created_at,r.created_by FROM task_doc_refs r JOIN documents d ON d.`key`=r.doc_key WHERE r.task_id=? ORDER BY r.doc_key"), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskDocRef{}
	for rows.Next() {
		var x TaskDocRef
		if err = rows.Scan(&x.TaskID, &x.Key, &x.Kind, &x.CreatedAt, &x.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// requireTaskRowTx refuses a task id with no task row. The caller holds the
// transaction, so the row cannot vanish between the check and the write.
func requireTaskRowTx(ctx context.Context, tx transaction, taskID int64) error {
	var n int64
	if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT COUNT(*) FROM tasks WHERE id=$1`, "SELECT COUNT(*) FROM tasks WHERE id=?"), taskID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// addTaskDocRefTx inserts one reference inside the caller's transaction. A
// reference that already exists is left as it is and reports false. The
// document must be stored; ErrDocNotFound otherwise.
func addTaskDocRefTx(ctx context.Context, tx transaction, taskID int64, key, by string, at time.Time) (bool, error) {
	var docs int64
	if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT COUNT(*) FROM documents WHERE key=$1`, "SELECT COUNT(*) FROM documents WHERE `key`=?"), key).Scan(&docs); err != nil {
		return false, err
	}
	if docs == 0 {
		return false, ErrDocNotFound
	}
	var existing int64
	if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT COUNT(*) FROM task_doc_refs WHERE task_id=$1 AND doc_key=$2`, "SELECT COUNT(*) FROM task_doc_refs WHERE task_id=? AND doc_key=?"), taskID, key).Scan(&existing); err != nil {
		return false, err
	}
	if existing > 0 {
		return false, nil
	}
	// The conflict clause only guards a concurrent insert of the same pair.
	if _, err := tx.Exec(ctx, sqlFor(tx,
		`INSERT INTO task_doc_refs(task_id,doc_key,created_at,created_by) VALUES($1,$2,$3,$4) ON CONFLICT (task_id,doc_key) DO NOTHING`,
		"INSERT INTO task_doc_refs(task_id,doc_key,created_at,created_by) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE task_id=task_id"),
		taskID, key, at, by); err != nil {
		return false, err
	}
	return true, nil
}

// deleteTaskDocRefsMySQLTx drops every task reference to the keys of documents
// the MySQL delete path removes in the same transaction. A reference has no
// foreign key to documents, so without this a re-created key would list the
// old citations (hk 6138). Call it after the documents delete. The PostgreSQL
// paths run the same statement inline (`= ANY($1)`).
func deleteTaskDocRefsMySQLTx(ctx context.Context, tx transaction, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	_, err := tx.Exec(ctx, "DELETE FROM task_doc_refs WHERE doc_key IN ("+placeholders(len(keys))+")", args...)
	return err
}

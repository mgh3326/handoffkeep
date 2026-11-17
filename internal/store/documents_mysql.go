package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// mysqlPutDocument runs after the common validation and hash computation.
// Deploy projections belong to the changes area and remain gated until ported.
func (s *Store) mysqlPutDocument(ctx context.Context, x Document, now time.Time) (Document, bool, error) {
	var out Document
	var changed bool
	err := s.retryTx(ctx, func() error {
		var err error
		out, changed, err = s.mysqlPutDocumentOnce(ctx, x, now)
		return err
	})
	return out, changed, err
}

func (s *Store) mysqlPutDocumentOnce(ctx context.Context, x Document, now time.Time) (Document, bool, error) {
	if strings.HasPrefix(x.Key, "deploy/") {
		return x, false, ErrDialectUnsupported
	}
	var hash string
	var task *int64
	var class *string
	err := s.db().QueryRow(ctx, "SELECT sha256,task_id,origin_class FROM documents WHERE `key`=?", x.Key).Scan(&hash, &task, &class)
	if err == nil && hash == x.SHA256 && (x.TaskID == nil || (task != nil && *task == *x.TaskID)) && (x.OriginClass == "" || (class != nil && *class == x.OriginClass)) {
		x.TaskID = task
		x.OriginClass = ""
		if class != nil {
			x.OriginClass = *class
		}
		return x, false, nil
	}
	if err != nil && !errors.Is(err, ErrNoRows) {
		return x, false, err
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return x, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if x.TaskID != nil {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, *x.TaskID).Scan(&exists); err != nil {
			return x, false, err
		}
		if !exists {
			return x, false, ErrTaskNotFound
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO documents(`key`,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,origin_class) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE kind=VALUES(kind),session=VALUES(session),job=VALUES(job),body=VALUES(body),sha256=VALUES(sha256),created_by=VALUES(created_by),updated_at=VALUES(updated_at),task_id=COALESCE(VALUES(task_id),task_id),origin_class=COALESCE(VALUES(origin_class),origin_class)", x.Key, x.Kind, x.Session, x.Job, x.Body, x.SHA256, x.CreatedBy, now, now, x.TaskID, nullIfEmpty(x.OriginClass))
	if err != nil {
		return x, true, err
	}
	err = tx.QueryRow(ctx, "SELECT id,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents WHERE `key`=?", x.Key).Scan(&x.ID, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass)
	if err != nil {
		return x, true, err
	}
	return x, true, tx.Commit(ctx)
}

func mysqlListDocumentsSQL(prefix, kind, session string, task int64, afterID *int64, limit int) (string, []any) {
	var where []string
	var args []any
	if prefix != "" {
		lo, hi := keyPrefixBounds(prefix)
		where = append(where, "`key` >= ? AND `key` < ?")
		args = append(args, lo, hi)
	}
	if kind != "" {
		where = append(where, "kind=?")
		args = append(args, kind)
	}
	if session != "" {
		where = append(where, "session=?")
		args = append(args, session)
	}
	if task > 0 {
		where = append(where, "task_id=?")
		args = append(args, task)
	}
	order := " ORDER BY `key`"
	if afterID != nil {
		where = append(where, "id>?")
		args = append(args, *afterID)
		order = " ORDER BY id ASC"
	}
	q := "SELECT id,`key`,kind,session,job,sha256,created_by,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	return q + order + " LIMIT ?", args
}

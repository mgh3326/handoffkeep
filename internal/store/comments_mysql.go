package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// mysqlCreateComment holds the same comment lock across the insert, the
// YouTrack enqueue and the commit, so a comment never escapes without its
// mirror (B4a, hk 5076).
func (s *Store) mysqlCreateComment(ctx context.Context, taskID int64, author, body string, w *MCPWrite) (CreatedTaskComment, bool, error) {
	var out CreatedTaskComment
	var replayed bool
	err := s.retryTx(ctx, func() error {
		var err error
		out, replayed, err = s.mysqlCreateCommentOnce(ctx, taskID, author, body, w)
		return err
	})
	return out, replayed, err
}

func (s *Store) mysqlCreateCommentOnce(ctx context.Context, taskID int64, author, body string, w *MCPWrite) (CreatedTaskComment, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return CreatedTaskComment{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
		return CreatedTaskComment{}, false, err
	}
	var c TaskComment
	if w != nil && w.Key != "" {
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT result FROM mcp_write_audit WHERE tool=? AND actor=? AND idempotency_key=?`, w.Tool, w.Actor, w.Key).Scan(&raw)
		if err == nil {
			if err = json.Unmarshal(raw, &c); err != nil {
				return CreatedTaskComment{}, false, err
			}
			if c.TaskID != taskID {
				return CreatedTaskComment{}, false, ErrIdempotencyKeyReused
			}
			projection, err := ytCommentProjectionTx(ctx, tx, c.ID)
			if err != nil {
				return CreatedTaskComment{}, false, err
			}
			return CreatedTaskComment{TaskComment: c, YouTrackProjection: &projection}, true, tx.Commit(ctx)
		}
		if !errors.Is(err, ErrNoRows) {
			return CreatedTaskComment{}, false, err
		}
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, taskID).Scan(&exists); err != nil {
		return CreatedTaskComment{}, false, err
	}
	if !exists {
		return CreatedTaskComment{}, false, ErrTaskNotFound
	}
	c = TaskComment{TaskID: taskID, Body: body, Author: author, CreatedAt: time.Now().UTC()}
	ref := ""
	if w != nil {
		ref = w.Key
	}
	c.ID, err = tx.InsertID(ctx, `INSERT INTO task_comments(task_id,body,author,created_at,source_ref) VALUES(?,?,?,?,?)`, taskID, body, author, c.CreatedAt, ref)
	if err != nil {
		return CreatedTaskComment{}, false, err
	}
	if w != nil {
		raw, e := json.Marshal(c)
		if e != nil {
			return CreatedTaskComment{}, false, e
		}
		_, err = tx.Exec(ctx, `INSERT INTO mcp_write_audit(tool,actor,channel,author,task_id,idempotency_key,result,at) VALUES(?,?,?,?,?,?,?,?)`, w.Tool, w.Actor, w.Channel, w.Author, taskID, w.Key, string(raw), c.CreatedAt)
		if errors.Is(err, errUniqueViolation) {
			return CreatedTaskComment{}, false, ErrIdempotencyKeyReused
		}
		if err != nil {
			return CreatedTaskComment{}, false, err
		}
		// The MySQL twin of the dots note in CreateMCPTaskCommentProjected (hk 6307).
		if w.Author == DotsLabel {
			if err = queueDotsCommentNoteTx(ctx, tx, c); err != nil {
				return CreatedTaskComment{}, false, err
			}
		}
	}
	op, enqueued, err := s.enqueueYTCommentTx(ctx, tx, c)
	if err != nil {
		return CreatedTaskComment{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CreatedTaskComment{}, false, err
	}
	logYTCommentEnqueued(c, op, enqueued)
	projection := ytProjectionOf(op, enqueued)
	return CreatedTaskComment{TaskComment: c, YouTrackProjection: &projection}, false, nil
}

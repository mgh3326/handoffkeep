package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// mysqlCreateComment holds the same comment lock across the insert and commit.
// Until the YouTrack area is ported, an enabled projection or authority table
// is refused before a write, so a comment cannot escape without its mirror.
func (s *Store) mysqlCreateComment(ctx context.Context, taskID int64, author, body string, w *MCPWrite) (CreatedTaskComment, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return CreatedTaskComment{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
		return CreatedTaskComment{}, false, err
	}
	hasAuthority, err := s.mysqlTableExists(ctx, tx, "yt_authority")
	if err != nil {
		return CreatedTaskComment{}, false, err
	}
	if hasAuthority || s.ytCommentsModeNow() != YTCommentsOff {
		return CreatedTaskComment{}, false, ErrDialectUnsupported
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
			projection := ytProjectionOf(ytwrite.Op{}, false)
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
	}
	if err = tx.Commit(ctx); err != nil {
		return CreatedTaskComment{}, false, err
	}
	projection := ytProjectionOf(ytwrite.Op{}, false)
	return CreatedTaskComment{TaskComment: c, YouTrackProjection: &projection}, false, nil
}

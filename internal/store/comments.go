package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// TaskCommentMaxBytes bounds one comment body. It matches the transition note
// limit so a comment can carry anything a note could.
const TaskCommentMaxBytes = MaxBytes

// TaskCommentListMax bounds one page of comments.
const TaskCommentListMax = 500

// taskCommentInsertLock serializes comment inserts. A BIGSERIAL id is issued
// before commit, so two concurrent writers could otherwise commit ids 11 and
// 12 in the order 12, 11 and a reader holding "last read = 12" would never see
// 11. Holding this transaction lock across nextval and commit makes id order
// equal visibility order, which is what an after_id cursor needs.
const taskCommentInsertLock = 824180046

var (
	ErrTaskCommentEmpty   = errors.New("comment_empty")
	ErrTaskCommentTooLong = errors.New("comment_too_long")
	ErrTaskCommentInvalid = errors.New("invalid_comment")
)

// TaskComment is one append-only note on a task. Comments are data only: they
// never change task state, priority, events, or lane events. A correction is a
// new comment. Author is always the authenticated client, never request data.
type TaskComment struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

const taskCommentColumns = `id,task_id,body,author,created_at`

// taskCommentMigrations are unconditional and idempotent. They deliberately
// do not claim a schema_version number: the table needs no gated DDL, and a
// number claimed here could collide with a gated migration developed in
// parallel and silently skip it.
var taskCommentMigrations = []string{
	`CREATE TABLE IF NOT EXISTS task_comments (id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id), body TEXT NOT NULL, author TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS task_comments_task_id ON task_comments(task_id, id ASC)`,
	`CREATE OR REPLACE FUNCTION handoffkeep_task_comments_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task_comments is append-only'; RETURN NULL; END; $$`,
	`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'task_comments_append_only' AND tgrelid = 'task_comments'::regclass) THEN CREATE TRIGGER task_comments_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON task_comments FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_task_comments_append_only(); END IF; END $$`,
}

func migrateTaskComments(ctx context.Context, tx pgx.Tx) error {
	for _, q := range taskCommentMigrations {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func scanTaskComment(row interface{ Scan(...any) error }, x *TaskComment) error {
	return row.Scan(&x.ID, &x.TaskID, &x.Body, &x.Author, &x.CreatedAt)
}

// ValidateTaskCommentBody reports the specific reason a body is unacceptable
// so callers can answer empty and oversized bodies with distinct responses.
func ValidateTaskCommentBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return ErrTaskCommentEmpty
	}
	if len(body) > TaskCommentMaxBytes {
		return ErrTaskCommentTooLong
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return ErrTaskCommentInvalid
	}
	return guard.Reject(body)
}

// CreateTaskComment appends one comment. The transaction writes task_comments
// only; tasks is read for existence and is never locked for update.
func (s *Store) CreateTaskComment(ctx context.Context, taskID int64, author, body string) (TaskComment, error) {
	if taskID < 1 || author == "" || !validText(author, 128) {
		return TaskComment{}, ErrTaskCommentInvalid
	}
	if err := ValidateTaskCommentBody(body); err != nil {
		return TaskComment{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TaskComment{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		return TaskComment{}, err
	}
	var x TaskComment
	err = scanTaskComment(tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) SELECT $1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM tasks WHERE id=$1) RETURNING `+taskCommentColumns, taskID, body, author, time.Now().UTC()), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskComment{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskComment{}, err
	}
	return x, tx.Commit(ctx)
}

// ListTaskComments returns comments in id order after afterID. A missing task
// is ErrTaskNotFound so callers never confuse it with a task without comments.
func (s *Store) ListTaskComments(ctx context.Context, taskID, afterID int64, limit int) ([]TaskComment, error) {
	if taskID < 1 || afterID < 0 || limit < 1 || limit > TaskCommentListMax {
		return nil, ErrTaskCommentInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrTaskNotFound
	}
	rows, err := tx.Query(ctx, `SELECT `+taskCommentColumns+` FROM task_comments WHERE task_id=$1 AND id>$2 ORDER BY id ASC LIMIT $3`, taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskComment{}
	for rows.Next() {
		var x TaskComment
		if err := scanTaskComment(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

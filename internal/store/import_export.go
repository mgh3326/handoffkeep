package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ImportCommentsPage returns a stable global comment page for the initial
// YouTrack import. Comment IDs are append-only and visibility ordered.
func (s *Store) ImportCommentsPage(ctx context.Context, afterID int64, limit int, project string, maxTaskID int64) ([]TaskComment, error) {
	if afterID < 0 || limit < 1 || limit > TaskCommentListMax || !validText(project, 128) || maxTaskID < 0 || (project == "" && maxTaskID != 0) {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.task_id,c.body,c.author,c.created_at FROM task_comments c JOIN tasks t ON t.id=c.task_id WHERE c.id>$1 AND ($3='' OR (t.project=$3 AND c.task_id<=$4)) ORDER BY c.id ASC LIMIT $2`, afterID, limit, project, maxTaskID)
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

// ImportDocumentsPage includes bodies and pages by immutable document ID.
func (s *Store) ImportDocumentsPage(ctx context.Context, project string, afterID int64, limit int) ([]Document, error) {
	if !validText(project, 128) || afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.key,d.kind,d.session,d.job,d.body,d.sha256,d.created_by,d.created_at,d.updated_at,d.task_id,COALESCE(d.origin_class,'') FROM documents d LEFT JOIN tasks t ON t.id=d.task_id WHERE d.id>$1 AND (($2='/shared' AND d.task_id IS NULL) OR ($2='/unassigned' AND d.task_id IS NOT NULL AND t.project IS NULL) OR ($2 NOT IN ('/shared','/unassigned') AND t.project=$2)) ORDER BY d.id ASC LIMIT $3`, afterID, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var x Document
		if err := rows.Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ImportDocumentAuthorsPage returns only document IDs and authors so the
// YouTrack importer can register users before importing articles without
// downloading every document body twice.
func (s *Store) ImportDocumentAuthorsPage(ctx context.Context, project string, afterID int64, limit int) ([]ImportDocumentAuthor, error) {
	if !validText(project, 128) || afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.created_by FROM documents d LEFT JOIN tasks t ON t.id=d.task_id WHERE d.id>$1 AND (($2='/shared' AND d.task_id IS NULL) OR ($2='/unassigned' AND d.task_id IS NOT NULL AND t.project IS NULL) OR ($2 NOT IN ('/shared','/unassigned') AND t.project=$2)) ORDER BY d.id ASC LIMIT $3`, afterID, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportDocumentAuthor{}
	for rows.Next() {
		var x ImportDocumentAuthor
		if err := rows.Scan(&x.ID, &x.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type ImportDocumentAuthor struct {
	ID        int64  `json:"id"`
	CreatedBy string `json:"created_by"`
}

// ImportDecisionsPage returns all decision records, including answered and
// superseded records, in immutable ID cursor order.
func (s *Store) ImportDecisionsPage(ctx context.Context, afterID int64, limit int, now time.Time) ([]DecisionRecord, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	// The regular decision reader filters by status; an import needs every
	// status, so use a read-only query with the same row scanner and derived fields.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE id>$1 ORDER BY id ASC LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRecord{}
	for rows.Next() {
		var x DecisionRecord
		if err := scanDecisionRecord(rows, &x, now.UTC()); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return out, tx.Commit(ctx)
}

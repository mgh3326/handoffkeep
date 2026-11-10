package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ImportComment is one comment row of the import export. HKOpID is set when
// hk itself writes the comment to YouTrack through the write protocol (slice
// b1a): the import must skip it, or the issue would get it twice.
type ImportComment struct {
	TaskComment
	HKOpID int64 `json:"hk_op_id,omitempty"`
}

// importOwnerSQL is the op that owns comment c (yt_comment_write_links), or 0.
const importOwnerSQL = `coalesce((SELECT w.op_id FROM yt_comment_write_links w WHERE w.comment_id=c.id),0)`

// ImportCommentsPage returns a stable global comment page for the initial
// YouTrack import. Comment IDs are append-only and visibility ordered.
func (s *Store) ImportCommentsPage(ctx context.Context, afterID int64, limit int, project string, maxTaskID int64) ([]ImportComment, error) {
	if afterID < 0 || limit < 1 || limit > TaskCommentListMax || !validText(project, 128) || maxTaskID < 0 || (project == "" && maxTaskID != 0) {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.task_id,c.body,c.author,c.created_at,`+importOwnerSQL+` FROM task_comments c JOIN tasks t ON t.id=c.task_id WHERE c.id>$1 AND ($3='' OR (t.project=$3 AND c.task_id<=$4)) ORDER BY c.id ASC LIMIT $2`, afterID, limit, project, maxTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportComment{}
	for rows.Next() {
		var x ImportComment
		if err := rows.Scan(&x.ID, &x.TaskID, &x.Body, &x.Author, &x.CreatedAt, &x.HKOpID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ImportUpdatedTask carries a task and its effective continuous-import cursor timestamp.
type ImportUpdatedTask struct {
	ImportTask
	ImportUpdatedAt time.Time `json:"import_updated_at"`
}

// ImportTask adds the names and translation that a YouTrack issue import needs
// without changing the ordinary task API shape.
type ImportTask struct {
	Task
	TitleKO       string   `json:"title_ko,omitempty"`
	LabelNames    []string `json:"label_names,omitempty"`
	MilestoneName string   `json:"milestone_name,omitempty"`
}

// EnrichImportTasks adds current Korean titles, label names, milestone names,
// and live relations to the export rows in one query per related table.
func (s *Store) EnrichImportTasks(ctx context.Context, tasks []Task) ([]ImportTask, error) {
	out := make([]ImportTask, len(tasks))
	if len(tasks) == 0 {
		return out, nil
	}
	ids := make([]int64, len(tasks))
	byID := make(map[int64]int, len(tasks))
	for i := range tasks {
		out[i].Task = tasks[i]
		ids[i] = tasks[i].ID
		byID[tasks[i].ID] = i
		out[i].Relations = []TaskRelation{}
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id,tr.text FROM tasks t JOIN translations tr ON tr.entity='task' AND tr.entity_id=t.id::text AND tr.field='title' AND tr.lang='ko' AND tr.source_sha256=encode(sha256(convert_to(t.title,'UTF8')),'hex') WHERE t.id=ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			rows.Close()
			return nil, err
		}
		out[byID[id]].TitleKO = title
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT tl.task_id,l.name FROM task_labels tl JOIN labels l ON l.id=tl.label_id WHERE tl.task_id=ANY($1::bigint[]) ORDER BY tl.task_id,l.name`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		out[byID[id]].LabelNames = append(out[byID[id]].LabelNames, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT t.id,m.name FROM tasks t JOIN project_milestones m ON m.id=t.milestone_id WHERE t.id=ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		out[byID[id]].MilestoneName = name
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT id,kind,src_id,dst_id,created_by,created_at FROM task_relations WHERE archived_at IS NULL AND (src_id=ANY($1::bigint[]) OR dst_id=ANY($1::bigint[])) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel TaskRelation
		if err := rows.Scan(&rel.ID, &rel.Kind, &rel.SrcID, &rel.DstID, &rel.CreatedBy, &rel.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byID[rel.SrcID]; ok {
			out[i].Relations = append(out[i].Relations, rel)
		}
		if i, ok := byID[rel.DstID]; ok && rel.DstID != rel.SrcID {
			out[i].Relations = append(out[i].Relations, rel)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return out, nil
}

// The initial YouTrack snapshot completed at 07:16 UTC. This one-time cursor
// floor re-sends every task with an import-comment or article-backlink document
// on the first continuous poll, then the persisted YouTrack cursor advances past it.
const importLinkedDocumentBackfillAt = "2026-11-09 07:17:00+00"

// ImportTasksUpdatedPage returns tasks changed since a timestamp/id keyset.
func (s *Store) ImportTasksUpdatedPage(ctx context.Context, project *string, afterAt time.Time, afterID int64, limit int) ([]ImportUpdatedTask, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+`,effective.import_updated_at
		FROM tasks LEFT JOIN LATERAL (SELECT max(updated_at) AS latest_at,
			(SELECT max(created_at) FROM task_comments WHERE task_id=tasks.id) AS latest_comment_at FROM documents WHERE task_id=tasks.id AND
			(key LIKE 'reports/mwr-%' OR key LIKE 'reports/%/report.md' OR key LIKE 'brief/%/%')) linked ON true
		CROSS JOIN LATERAL (SELECT GREATEST(tasks.updated_at,tasks.last_activity_at,COALESCE(linked.latest_at,tasks.last_activity_at),
			COALESCE(linked.latest_comment_at,tasks.last_activity_at),
			CASE WHEN linked.latest_at IS NULL THEN tasks.last_activity_at ELSE TIMESTAMPTZ '`+importLinkedDocumentBackfillAt+`' END) AS import_updated_at) effective
		WHERE ($1::text IS NULL OR tasks.project IS NOT DISTINCT FROM NULLIF($1,''))
		AND (effective.import_updated_at,tasks.id)>($2,$3)
		ORDER BY effective.import_updated_at,tasks.id LIMIT $4`, project, afterAt.UTC(), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportUpdatedTask{}
	for rows.Next() {
		var x ImportUpdatedTask
		if err := scanTask(rows, &x.Task, &x.ImportUpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	base := make([]Task, len(out))
	for i := range out {
		base[i] = out[i].Task
	}
	enriched, err := s.EnrichImportTasks(ctx, base)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ImportTask = enriched[i]
	}
	return out, nil
}

// ImportTaskDocumentsPage returns task documents that become issue comments or article backlinks.
func (s *Store) ImportTaskDocumentsPage(ctx context.Context, taskIDs []int64, afterID int64, limit int) ([]Document, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	ids := make([]int64, len(taskIDs))
	copy(ids, taskIDs)
	if len(ids) == 0 {
		return []Document{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,COALESCE(origin_class,'') FROM documents WHERE task_id=ANY($1::bigint[]) AND id>$2 AND (key LIKE 'reports/mwr-%' OR key LIKE 'reports/%/report.md' OR key LIKE 'brief/%/%') ORDER BY id LIMIT $3`, ids, afterID, limit)
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

// ImportCommentsUpdatedPage returns comments for changed tasks or after a creation cursor.
func (s *Store) ImportCommentsUpdatedPage(ctx context.Context, taskIDs []int64, afterAt time.Time, afterID int64, limit int) ([]ImportComment, error) {
	if afterID < 0 || limit < 1 || limit > TaskCommentListMax {
		return nil, errors.New("invalid import cursor")
	}
	ids := make([]int64, len(taskIDs))
	copy(ids, taskIDs)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.task_id,c.body,c.author,c.created_at,`+importOwnerSQL+` FROM task_comments c WHERE (cardinality($1::bigint[])=0 OR c.task_id=ANY($1)) AND (c.created_at,c.id)>($2,$3) ORDER BY c.created_at,c.id LIMIT $4`, ids, afterAt.UTC(), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportComment{}
	for rows.Next() {
		var x ImportComment
		if err := rows.Scan(&x.ID, &x.TaskID, &x.Body, &x.Author, &x.CreatedAt, &x.HKOpID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type ImportCommentAuthor struct {
	Author string `json:"author"`
}

// ImportCommentAuthorsPage keeps the full-import user directory complete without loading comment bodies.
func (s *Store) ImportCommentAuthorsPage(ctx context.Context, after string, limit int) ([]ImportCommentAuthor, error) {
	if limit < 1 || limit > 1000 || !validText(after, 128) {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT author FROM task_comments WHERE author>$1 ORDER BY author LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportCommentAuthor{}
	for rows.Next() {
		var x ImportCommentAuthor
		if err := rows.Scan(&x.Author); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ImportDocumentsUpdatedPage returns current documents changed since the timestamp/id keyset.
func (s *Store) ImportDocumentsUpdatedPage(ctx context.Context, project string, afterAt time.Time, afterID int64, limit int) ([]Document, error) {
	if !validText(project, 128) || afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.key,d.kind,d.session,d.job,d.body,d.sha256,d.created_by,d.created_at,d.updated_at,d.task_id,COALESCE(d.origin_class,'') FROM documents d LEFT JOIN tasks t ON t.id=d.task_id WHERE (($1='/shared' AND d.task_id IS NULL) OR ($1='/unassigned' AND d.task_id IS NOT NULL AND t.project IS NULL) OR ($1 NOT IN ('/shared','/unassigned') AND t.project=$1)) AND (d.updated_at,d.id)>($2,$3) ORDER BY d.updated_at,d.id LIMIT $4`, project, afterAt.UTC(), afterID, limit)
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

// ImportDecisionsUpdatedPage returns decisions whose latest stored change is after the cursor.
func (s *Store) ImportDecisionsUpdatedPage(ctx context.Context, afterAt time.Time, afterID int64, limit int, now time.Time) ([]DecisionRecord, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid import cursor")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE (updated_at,id)>($1,$2) ORDER BY updated_at,id LIMIT $3`, afterAt.UTC(), afterID, limit)
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
		return nil, err
	}
	rows.Close()
	return out, tx.Commit(ctx)
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

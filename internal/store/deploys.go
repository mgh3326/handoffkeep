package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ListDocumentsByPrefix returns documents whose key starts with prefix,
// newest key first, including bodies — unlike ListDocuments, which is a
// metadata listing. The match is a literal strpos prefix, never LIKE, so a
// caller pattern can never widen the scan past the intended key space.
func (s *Store) ListDocumentsByPrefix(ctx context.Context, prefix string, limit int) ([]Document, error) {
	if prefix == "" || !validText(prefix, 512) {
		return nil, errors.New("invalid document prefix")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at FROM documents WHERE strpos(key, $1) = 1 ORDER BY key DESC LIMIT $2`, prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var x Document
		if err = rows.Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// MergedTaskEvent is one task's merge transition with the refs snapshot the
// transition recorded. merged is terminal in TaskTransitions, so a task has
// at most one of these; pre-v5 events may carry a NULL refs snapshot.
type MergedTaskEvent struct {
	TaskID int64
	Title  string
	At     time.Time
	Refs   *TaskRefs
}

func (s *Store) ListMergedTaskEvents(ctx context.Context, limit int) ([]MergedTaskEvent, error) {
	if limit < 1 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	rows, err := s.pool.Query(ctx, `SELECT e.task_id, t.title, e.at, e.refs FROM task_events e JOIN tasks t ON t.id = e.task_id WHERE e."to"='merged' ORDER BY e.at DESC, e.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MergedTaskEvent{}
	for rows.Next() {
		var x MergedTaskEvent
		var refs []byte
		if err = rows.Scan(&x.TaskID, &x.Title, &x.At, &refs); err != nil {
			return nil, err
		}
		if refs != nil {
			var parsed TaskRefs
			if err = json.Unmarshal(refs, &parsed); err != nil {
				return nil, err
			}
			x.Refs = &parsed
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

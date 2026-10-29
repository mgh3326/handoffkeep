package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ListDocumentsByPrefix returns documents whose key starts with prefix,
// newest key first, including bodies — unlike ListDocuments, which is a
// metadata listing. The match is a literal strpos prefix, never LIKE, so a
// caller pattern can never widen the scan past the intended key space.
func (s *Store) ListDocumentsByPrefix(ctx context.Context, prefix string, limit int) ([]Document, error) {
	return s.listDocumentsByPrefix(ctx, prefix, "", limit)
}

// ListDocumentsByPrefixBefore continues a ListDocumentsByPrefix scan strictly
// below before (key ordering), so a caller whose bounded newest-first window
// came back full can page deeper instead of treating the window as the world.
func (s *Store) ListDocumentsByPrefixBefore(ctx context.Context, prefix, before string, limit int) ([]Document, error) {
	if before == "" {
		return nil, errors.New("missing cursor key")
	}
	return s.listDocumentsByPrefix(ctx, prefix, before, limit)
}

func (s *Store) listDocumentsByPrefix(ctx context.Context, prefix, before string, limit int) ([]Document, error) {
	if prefix == "" || !validText(prefix, 512) {
		return nil, errors.New("invalid document prefix")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var rows pgx.Rows
	var err error
	if before == "" {
		rows, err = s.pool.Query(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at FROM documents WHERE strpos(key, $1) = 1 ORDER BY key DESC LIMIT $2`, prefix, limit)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT id,key,kind,session,job,body,sha256,created_by,created_at,updated_at FROM documents WHERE strpos(key, $1) = 1 AND key < $2 ORDER BY key DESC LIMIT $3`, prefix, before, limit)
	}
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

// documentSummaryHeadChars bounds the leading characters a listing read
// carries for title extraction. Postgres counts characters, so the byte size
// stays bounded at 4x this for UTF-8 — never the whole body.
const documentSummaryHeadChars = 8 * 1024

// DocumentSummary is one listing row for the console's document browser. Head
// carries only the leading body characters (title extraction); Body is never
// listed, so a prefix scan cannot pull whole reports into memory.
type DocumentSummary struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Session   string    `json:"session"`
	Size      int64     `json:"size"`
	Head      string    `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DocumentPrefixCount is one top-level key prefix and how many documents it
// holds. Prefix keeps the separator ("policy/") so it is directly reusable as
// a list prefix; a slashless key is its own prefix.
type DocumentPrefixCount struct {
	Prefix string `json:"prefix"`
	Count  int64  `json:"count"`
}

// ListDocumentSummariesByPrefix pages documents whose key starts with prefix,
// newest key first, reading only metadata plus a bounded body head. An empty
// prefix matches every document. before continues the scan strictly below a
// key already seen, so a full page can always page deeper.
func (s *Store) ListDocumentSummariesByPrefix(ctx context.Context, prefix, before string, limit int) ([]DocumentSummary, error) {
	if !validText(prefix, 512) {
		return nil, errors.New("invalid document prefix")
	}
	if before != "" && !validText(before, 512) {
		return nil, errors.New("invalid document cursor")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT key,kind,session,octet_length(body),substr(body,1,$4),updated_at FROM documents WHERE ($1='' OR strpos(key,$1)=1) AND ($2='' OR key<$2) ORDER BY key DESC LIMIT $3`, prefix, before, limit, documentSummaryHeadChars)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocumentSummary{}
	for rows.Next() {
		var x DocumentSummary
		if err = rows.Scan(&x.Key, &x.Kind, &x.Session, &x.Size, &x.Head, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListDocumentPrefixCounts groups every document by its top-level key prefix.
// The console renders this as the browse tree; counts are of the whole key
// space, not of any one page.
func (s *Store) ListDocumentPrefixCounts(ctx context.Context) ([]DocumentPrefixCount, error) {
	rows, err := s.pool.Query(ctx, `SELECT CASE WHEN strpos(key,'/')>0 THEN left(key,strpos(key,'/')) ELSE key END AS prefix, count(*)::bigint FROM documents GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocumentPrefixCount{}
	for rows.Next() {
		var x DocumentPrefixCount
		if err = rows.Scan(&x.Prefix, &x.Count); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// MergedTaskEvent is one task's merge transition with the refs snapshot the
// transition recorded. Only the latest merge of a currently merged task is
// returned; pre-v5 events may carry a NULL refs snapshot.
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
	rows, err := s.pool.Query(ctx, `SELECT e.task_id, t.title, e.at, e.refs FROM tasks t JOIN LATERAL (SELECT task_id, at, refs, id FROM task_events WHERE task_id=t.id AND kind='transition' AND "to"='merged' ORDER BY at DESC, id DESC LIMIT 1) e ON true WHERE t.state='merged' ORDER BY e.at DESC, e.id DESC LIMIT $1`, limit)
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

package store

import (
	"context"
	"encoding/json"
	"time"
)

const maxDocumentPrefixBytes = 512

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
	if s != nil && s.dialect == mysqlDialect {
		return nil, ErrDialectUnsupported
	}

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

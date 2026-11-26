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

// listMergedTaskEventsSQL (PostgreSQL) drives from the merge transitions,
// newest first: each row kept is a task's latest merge and the task is still
// merged. The outer equality and order ride task_events_kind_to_at_id, the
// NOT EXISTS probe rides task_events_task_at (filtered on kind and "to"), and
// tasks is a primary-key join, so the read stops after the limit instead of
// walking every merged task first.
const listMergedTaskEventsSQL = `SELECT e.task_id, t.title, e.at, e.refs FROM task_events e JOIN tasks t ON t.id=e.task_id
	WHERE e.kind='transition' AND e."to"='merged' AND t.state='merged'
	AND NOT EXISTS (SELECT 1 FROM task_events n WHERE n.task_id=e.task_id AND n.kind='transition' AND n."to"='merged' AND (n.at > e.at OR (n.at = e.at AND n.id > e.id)))
	ORDER BY e.at DESC, e.id DESC LIMIT $1`

// listMergedTaskEventsMySQL (MySQL, production) drives from currently merged
// tasks (tasks_state_id), then probes one newest merge per task through
// task_events_task_kind_to_at. It does not walk the merge transitions, so the
// rows examined track the merged backlog per task, not the merge history.
const listMergedTaskEventsMySQL = `SELECT e.task_id, t.title, e.at, e.refs FROM tasks t
	JOIN LATERAL (SELECT id, task_id, at, refs FROM task_events
		WHERE task_id=t.id AND kind='transition' AND ` + "`to`" + `='merged'
		ORDER BY at DESC, id DESC LIMIT 1) e ON true
	WHERE t.state='merged'
	ORDER BY e.at DESC, e.id DESC LIMIT ?`

func (s *Store) ListMergedTaskEvents(ctx context.Context, limit int) ([]MergedTaskEvent, error) {
	if limit < 1 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	rows, err := s.db().Query(ctx, s.q(listMergedTaskEventsSQL, listMergedTaskEventsMySQL), limit)
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

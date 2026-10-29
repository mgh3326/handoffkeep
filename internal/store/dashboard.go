package store

import (
	"context"
	"encoding/json"
	"time"
)

// Read-only queries behind the console home dashboard (hk 2280). They read the
// existing quota and task tables; nothing here writes or migrates.

// QuotaAccountLatest is one account's latest pointer set, as LatestQuota
// returns it for a single key.
type QuotaAccountLatest struct {
	AccountKey string `json:"account_key"`
	QuotaLatest
}

// ListLatestQuota returns the latest automatic and manual observation of every
// account in quota_latest, ordered by account key. limit bounds the pointer
// rows read; truncated reports that more rows exist.
func (s *Store) ListLatestQuota(ctx context.Context, limit int) ([]QuotaAccountLatest, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT l.account_key,o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id ORDER BY l.account_key,l.kind,l.window_key LIMIT $1`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []QuotaAccountLatest{}
	n := 0
	truncated := false
	for rows.Next() {
		if n == limit {
			truncated = true
			break
		}
		n++
		var key string
		var id int64
		var data []byte
		var confirmedAt *time.Time
		if err = rows.Scan(&key, &id, &data, &confirmedAt); err != nil {
			return nil, false, err
		}
		var x QuotaObservation
		if err = json.Unmarshal(data, &x); err != nil {
			return nil, false, err
		}
		x.ID = id
		x.ConfirmedAt = confirmedAt
		if len(out) == 0 || out[len(out)-1].AccountKey != key {
			out = append(out, QuotaAccountLatest{AccountKey: key, QuotaLatest: QuotaLatest{Manual: []QuotaObservation{}}})
		}
		cur := &out[len(out)-1]
		if x.Kind == "auto" {
			cur.Observation = &x
		} else {
			cur.Manual = append(cur.Manual, x)
		}
	}
	return out, truncated, rows.Err()
}

// ListDashboardTasks returns the tasks the dashboard shows as running work or
// open pull requests: every task in an active state, plus any other unfinished
// task that carries refs.pr. Newest update first, bounded by limit; truncated
// reports that more rows exist.
func (s *Store) ListDashboardTasks(ctx context.Context, limit int) ([]Task, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE state IN ('claimed','in_progress','verifying','join')
		   OR (state NOT IN ('merged','dropped') AND COALESCE(refs->>'pr','') <> '')
		ORDER BY updated_at DESC,id DESC LIMIT $1`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		if len(out) == limit {
			return out, true, nil
		}
		var task Task
		if err := scanTask(rows, &task); err != nil {
			return nil, false, err
		}
		out = append(out, task)
	}
	return out, false, rows.Err()
}

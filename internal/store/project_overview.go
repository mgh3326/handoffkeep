package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectOverview is one project's owner, metadata and open task counts. It is
// the read model behind the MCP projects_list tool: the owner fields come from
// the same store rule as GET /v1/tasks/projects/owners.
type ProjectOverview struct {
	ProjectOwner
	Status   string     `json:"status"`
	StartAt  *time.Time `json:"start_at"`
	TargetAt *time.Time `json:"target_at"`
	// OpenCounts is the project's open tasks per state. Settled states are
	// excluded; the state list itself is TaskStateMachine.
	OpenCounts map[string]int `json:"open_task_counts"`
	OpenTotal  int            `json:"open_total"`
}

// ListProjectOverviews returns every configured project in name order with its
// owner, metadata and open task counts. The two reads share one snapshot.
func (s *Store) ListProjectOverviews(ctx context.Context) ([]ProjectOverview, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT p.name,p.design_lane,e.created_at,p.status,p.start_at,p.target_at
		FROM task_projects p
		LEFT JOIN LATERAL (SELECT created_at FROM project_owner_events WHERE project = p.name ORDER BY id DESC LIMIT 1) e ON true
		ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	out := []ProjectOverview{}
	index := map[string]int{}
	for rows.Next() {
		var p ProjectOverview
		if err := rows.Scan(&p.Name, &p.DesignLane, &p.UpdatedAt, &p.Status, &p.StartAt, &p.TargetAt); err != nil {
			rows.Close()
			return nil, err
		}
		p.EffectiveOwner = effectiveProjectOwner(p.DesignLane)
		p.OpenCounts = map[string]int{}
		index[p.Name] = len(out)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `SELECT project,state,COUNT(*) FROM tasks
		WHERE project IS NOT NULL AND state NOT IN (`+settledStatesSQL+`)
		GROUP BY project,state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project, state string
		var count int
		if err := rows.Scan(&project, &state, &count); err != nil {
			return nil, err
		}
		if i, ok := index[project]; ok {
			out[i].OpenCounts[state] = count
			out[i].OpenTotal += count
		}
	}
	return out, rows.Err()
}

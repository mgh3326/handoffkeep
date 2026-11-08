package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project health bounds: how many assignees and reason rows one project
// reports. The counts are never bounded; only the listed names and reasons are.
const (
	projectAssigneeLimit = 10
	projectReasonLimit   = 5
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
	Health     ProjectHealth  `json:"health"`
}

// ProjectHealth summarises a project's work for a reader deciding what needs
// attention. The buckets overlap by design: a blocked task that is also in
// progress counts in both. Open means not settled; done is the completed
// category (merged, deployed, completed) and dropped or duplicate work is in
// neither bucket.
type ProjectHealth struct {
	InProgress int `json:"in_progress"`
	Waiting    int `json:"waiting"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
	Overdue    int `json:"overdue"`
	// Assignees are the claimers of open tasks, most tasks first, bounded by
	// projectAssigneeLimit; AssigneesMore reports a cut list.
	Assignees     []ProjectAssignee `json:"assignees"`
	AssigneesMore bool              `json:"assignees_more"`
	// TopBlocked and TopOverdue hold the highest-priority blocked tasks (their
	// open blockers named) and the earliest overdue tasks, each bounded by
	// projectReasonLimit.
	TopBlocked []ProjectBlockedTask `json:"top_blocked"`
	TopOverdue []ProjectOverdueTask `json:"top_overdue"`
}

// ProjectAssignee is one claimer and how many open tasks they hold.
type ProjectAssignee struct {
	Name string `json:"name"`
	Open int    `json:"open"`
}

// ProjectBlockedTask is a blocked open task with the open tasks blocking it.
type ProjectBlockedTask struct {
	TaskID    int64   `json:"task_id"`
	Title     string  `json:"title"`
	Priority  int     `json:"priority"`
	BlockedBy []int64 `json:"blocked_by"`
}

// ProjectOverdueTask is an open task whose due date has passed at as_of.
type ProjectOverdueTask struct {
	TaskID   int64     `json:"task_id"`
	Title    string    `json:"title"`
	Priority int       `json:"priority"`
	DueAt    time.Time `json:"due_at"`
}

// openBlockedSQL is true for a task (alias t) with at least one unarchived
// blocks edge from a blocker that is still open.
const openBlockedSQL = `EXISTS (SELECT 1 FROM task_relations r JOIN tasks b ON b.id=r.src_id
	WHERE r.kind='blocks' AND r.dst_id=t.id AND r.archived_at IS NULL AND b.state NOT IN (` + settledStatesSQL + `))`

// ListProjectOverviews returns every configured project in name order with its
// owner, metadata, open task counts and health. now is the as-of instant for
// overdue tasks. The reads share one snapshot.
func (s *Store) ListProjectOverviews(ctx context.Context, now time.Time) ([]ProjectOverview, error) {
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
		p.Health.Assignees = []ProjectAssignee{}
		p.Health.TopBlocked = []ProjectBlockedTask{}
		p.Health.TopOverdue = []ProjectOverdueTask{}
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
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := projectHealthCounts(ctx, tx, now, out, index); err != nil {
		return nil, err
	}
	if err := projectAssigneeRows(ctx, tx, out, index); err != nil {
		return nil, err
	}
	if err := projectBlockedRows(ctx, tx, out, index); err != nil {
		return nil, err
	}
	if err := projectOverdueRows(ctx, tx, now, out, index); err != nil {
		return nil, err
	}
	return out, nil
}

func projectHealthCounts(ctx context.Context, tx pgx.Tx, now time.Time, out []ProjectOverview, index map[string]int) error {
	rows, err := tx.Query(ctx, `SELECT t.project,
		COUNT(*) FILTER (WHERE t.state IN (`+startedStatesSQL+`)),
		COUNT(*) FILTER (WHERE t.state IN ('triage','backlog','unstarted')),
		COUNT(*) FILTER (WHERE t.state IN ('merged','deployed','completed')),
		COUNT(*) FILTER (WHERE t.state NOT IN (`+settledStatesSQL+`) AND `+openBlockedSQL+`),
		COUNT(*) FILTER (WHERE t.state NOT IN (`+settledStatesSQL+`) AND t.due_at IS NOT NULL AND t.due_at < $1)
		FROM tasks t WHERE t.project IS NOT NULL GROUP BY t.project`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var h ProjectHealth
		if err := rows.Scan(&project, &h.InProgress, &h.Waiting, &h.Done, &h.Blocked, &h.Overdue); err != nil {
			return err
		}
		if i, ok := index[project]; ok {
			out[i].Health.InProgress = h.InProgress
			out[i].Health.Waiting = h.Waiting
			out[i].Health.Done = h.Done
			out[i].Health.Blocked = h.Blocked
			out[i].Health.Overdue = h.Overdue
		}
	}
	return rows.Err()
}

func projectAssigneeRows(ctx context.Context, tx pgx.Tx, out []ProjectOverview, index map[string]int) error {
	rows, err := tx.Query(ctx, `SELECT project,claimed_by,COUNT(*) FROM tasks
		WHERE project IS NOT NULL AND claimed_by<>'' AND state NOT IN (`+settledStatesSQL+`)
		GROUP BY project,claimed_by ORDER BY project,COUNT(*) DESC,claimed_by`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var a ProjectAssignee
		if err := rows.Scan(&project, &a.Name, &a.Open); err != nil {
			return err
		}
		i, ok := index[project]
		if !ok {
			continue
		}
		h := &out[i].Health
		if len(h.Assignees) >= projectAssigneeLimit {
			h.AssigneesMore = true
			continue
		}
		h.Assignees = append(h.Assignees, a)
	}
	return rows.Err()
}

func projectBlockedRows(ctx context.Context, tx pgx.Tx, out []ProjectOverview, index map[string]int) error {
	rows, err := tx.Query(ctx, `SELECT project,id,title,priority,blocked_by FROM (
		SELECT t.project,t.id,t.title,t.priority,
		  (SELECT ARRAY_AGG(b.id ORDER BY b.id) FROM task_relations r JOIN tasks b ON b.id=r.src_id
		     WHERE r.kind='blocks' AND r.dst_id=t.id AND r.archived_at IS NULL AND b.state NOT IN (`+settledStatesSQL+`)) AS blocked_by,
		  ROW_NUMBER() OVER (PARTITION BY t.project ORDER BY t.priority DESC,t.id) AS rn
		FROM tasks t WHERE t.project IS NOT NULL AND t.state NOT IN (`+settledStatesSQL+`) AND `+openBlockedSQL+`
		) x WHERE rn <= $1 ORDER BY project,rn`, projectReasonLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var b ProjectBlockedTask
		if err := rows.Scan(&project, &b.TaskID, &b.Title, &b.Priority, &b.BlockedBy); err != nil {
			return err
		}
		if i, ok := index[project]; ok {
			out[i].Health.TopBlocked = append(out[i].Health.TopBlocked, b)
		}
	}
	return rows.Err()
}

func projectOverdueRows(ctx context.Context, tx pgx.Tx, now time.Time, out []ProjectOverview, index map[string]int) error {
	rows, err := tx.Query(ctx, `SELECT project,id,title,priority,due_at FROM (
		SELECT t.project,t.id,t.title,t.priority,t.due_at,
		  ROW_NUMBER() OVER (PARTITION BY t.project ORDER BY t.due_at,t.id) AS rn
		FROM tasks t WHERE t.project IS NOT NULL AND t.state NOT IN (`+settledStatesSQL+`)
		  AND t.due_at IS NOT NULL AND t.due_at < $1
		) x WHERE rn <= $2 ORDER BY project,rn`, now, projectReasonLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var o ProjectOverdueTask
		if err := rows.Scan(&project, &o.TaskID, &o.Title, &o.Priority, &o.DueAt); err != nil {
			return err
		}
		if i, ok := index[project]; ok {
			out[i].Health.TopOverdue = append(out[i].Health.TopOverdue, o)
		}
	}
	return rows.Err()
}

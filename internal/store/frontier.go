package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Frontier struct {
	GeneratedAt  time.Time        `json:"generated_at"`
	Project      string           `json:"project"`
	Since        time.Time        `json:"since"`
	Truncated    bool             `json:"truncated"`
	Counts       FrontierCounts   `json:"counts"`
	Changes      []FrontierChange `json:"changes"`
	ChangedSince FrontierChanged  `json:"changed_since"`
	Gates        FrontierGates    `json:"gates"`
}
type FrontierCounts struct {
	Landable  int `json:"landable"`
	Blocked   int `json:"blocked"`
	Waiting   int `json:"waiting"`
	Verifying int `json:"verifying"`
	Total     int `json:"total"`
}
type FrontierChange struct {
	ChangeID int64      `json:"change_id"`
	Repo     string     `json:"repo"`
	PRNumber int        `json:"pr_number"`
	PR       string     `json:"pr"`
	Class    string     `json:"class"`
	Reasons  []string   `json:"reasons"`
	Tasks    []int64    `json:"tasks"`
	Priority int        `json:"priority"`
	HeadSHA  *string    `json:"head_sha"`
	OpenedAt time.Time  `json:"opened_at"`
	MergedAt *time.Time `json:"merged_at"`
}
type FrontierChanged struct {
	Count int                   `json:"count"`
	Items []FrontierChangedItem `json:"items"`
}
type FrontierChangedItem struct {
	ChangeID int64  `json:"change_id"`
	PR       string `json:"pr"`
	What     string `json:"what"`
}
type FrontierGates struct {
	DecisionsOpen int `json:"decisions_open"`
	Held          int `json:"held"`
	Findings      int `json:"findings"`
}

type frontierRow struct {
	FrontierChange
	state    string
	hold     *string
	depID    *int64
	depState *string
	updated  time.Time
}

// ProjectFrontier derives the project view from one read-only snapshot.
func (s *Store) ProjectFrontier(ctx context.Context, project string, since, now time.Time) (Frontier, error) {
	now, since = now.UTC(), since.UTC()
	out := Frontier{GeneratedAt: now, Project: project, Since: since, Changes: []FrontierChange{}, ChangedSince: FrontierChanged{Items: []FrontierChangedItem{}}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, project).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrProjectNotFound
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM decision_requests WHERE `+decisionOpenSQL+` AND task_id IN (SELECT id FROM tasks WHERE project=$2)`, now, project).Scan(&out.Gates.DecisionsOpen); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id,c.repo,c.pr_number,c.state,c.head_sha,c.opened_at,c.merged_at,c.priority,c.hold_reason,c.depends_on_change_id,c.depends_on_state,c.updated_at
 FROM changes c WHERE EXISTS (SELECT 1 FROM change_tasks ct JOIN tasks t ON t.id=ct.task_id WHERE ct.change_id=c.id AND ct.relation='implements' AND t.project=$1)
 AND (c.state='open' OR (c.state='merged' AND c.reverted_by_change_id IS NULL AND EXISTS (SELECT 1 FROM change_tasks ct JOIN tasks t ON t.id=ct.task_id WHERE ct.change_id=c.id AND ct.relation='implements' AND t.project=$1 AND t.state='merged')))
 ORDER BY c.priority DESC,c.opened_at ASC,c.id ASC LIMIT 501`, project)
	if err != nil {
		return out, err
	}
	var selected []frontierRow
	for rows.Next() {
		var x frontierRow
		if err = rows.Scan(&x.ChangeID, &x.Repo, &x.PRNumber, &x.state, &x.HeadSHA, &x.OpenedAt, &x.MergedAt, &x.Priority, &x.hold, &x.depID, &x.depState, &x.updated); err != nil {
			rows.Close()
			return out, err
		}
		selected = append(selected, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(selected) > 500 {
		out.Truncated = true
		selected = selected[:500]
	}
	for _, x := range selected {
		x.PR = changeURL(x.Repo, x.PRNumber)
		x.Reasons = []string{}
		x.Tasks = []int64{}
		blocked, waiting := []string{}, []string{}
		if x.hold != nil && *x.hold != "" {
			blocked = append(blocked, "hold:"+*x.hold)
		}
		taskRows, e := tx.Query(ctx, `SELECT t.id,t.state,t.awaiting_decision FROM change_tasks ct JOIN tasks t ON t.id=ct.task_id WHERE ct.change_id=$1 AND ct.relation='implements' ORDER BY t.id`, x.ChangeID)
		if e != nil {
			return out, e
		}
		for taskRows.Next() {
			var id int64
			var state string
			var awaiting bool
			if e = taskRows.Scan(&id, &state, &awaiting); e != nil {
				taskRows.Close()
				return out, e
			}
			x.Tasks = append(x.Tasks, id)
			if state == "dropped" || state == "duplicate" {
				blocked = append(blocked, fmt.Sprintf("task_closed:%d", id))
			}
			if awaiting {
				waiting = append(waiting, fmt.Sprintf("decision:task%d", id))
			}
		}
		e = taskRows.Err()
		taskRows.Close()
		if e != nil {
			return out, e
		}
		var critical, high int
		var unknown bool
		var latest *time.Time
		e = tx.QueryRow(ctx, `SELECT COALESCE(sum(critical),0),COALESCE(sum(high),0),COALESCE(bool_or(critical IS NULL OR high IS NULL),true) FROM (SELECT DISTINCT ON (lens) critical,high FROM reviews WHERE change_id=$1 AND reviewed_head=$2 ORDER BY lens,round DESC,pass DESC,id DESC) r`, x.ChangeID, x.HeadSHA).Scan(&critical, &high, &unknown)
		if e != nil {
			return out, e
		}
		if critical > 0 || high > 0 {
			blocked = append(blocked, fmt.Sprintf("findings:c%dh%d", critical, high))
		}
		if x.HeadSHA == nil || unknown {
			waiting = append(waiting, "review")
		}
		if x.depID != nil {
			var satisfied bool
			// Until deploy records land (hk 3036), deployed is derived from task state.
			e = tx.QueryRow(ctx, `SELECT CASE WHEN $2='merged' THEN state='merged' ELSE EXISTS(SELECT 1 FROM change_tasks WHERE change_id=$1 AND relation='implements') AND NOT EXISTS(SELECT 1 FROM change_tasks ct JOIN tasks t ON t.id=ct.task_id WHERE ct.change_id=$1 AND ct.relation='implements' AND t.state<>'deployed') END FROM changes WHERE id=$1`, *x.depID, *x.depState).Scan(&satisfied)
			if e != nil {
				return out, e
			}
			if !satisfied {
				waiting = append(waiting, fmt.Sprintf("change:%d:%s", *x.depID, *x.depState))
			}
		}
		dr, e := tx.Query(ctx, `SELECT id FROM decision_requests WHERE `+decisionOpenSQL+` AND task_id IN (SELECT task_id FROM change_tasks WHERE change_id=$2) ORDER BY id`, now, x.ChangeID)
		if e != nil {
			return out, e
		}
		for dr.Next() {
			var id int64
			if e = dr.Scan(&id); e != nil {
				dr.Close()
				return out, e
			}
			waiting = append(waiting, fmt.Sprintf("decision:%d", id))
		}
		e = dr.Err()
		dr.Close()
		if e != nil {
			return out, e
		}
		e = tx.QueryRow(ctx, `SELECT max(recorded_at) FROM reviews WHERE change_id=$1`, x.ChangeID).Scan(&latest)
		if e != nil {
			return out, e
		}
		if x.updated.After(since) || (latest != nil && latest.After(since)) {
			what := "updated"
			if latest != nil && !latest.Before(x.updated) {
				what = "reviewed"
			}
			if x.MergedAt != nil && x.MergedAt.After(since) && (latest == nil || !latest.After(*x.MergedAt)) {
				what = "merged"
			}
			out.ChangedSince.Count++
			if len(out.ChangedSince.Items) < 10 {
				out.ChangedSince.Items = append(out.ChangedSince.Items, FrontierChangedItem{x.ChangeID, x.PR, what})
			}
		}
		switch {
		case x.state == "merged":
			x.Class = "verifying"
			out.Counts.Verifying++
		case len(blocked) > 0:
			x.Class = "blocked"
			x.Reasons = blocked
			out.Counts.Blocked++
			if x.hold != nil && *x.hold != "" {
				out.Gates.Held++
			}
			if critical > 0 || high > 0 {
				out.Gates.Findings++
			}
		case len(waiting) > 0:
			x.Class = "waiting"
			x.Reasons = waiting
			out.Counts.Waiting++
		default:
			x.Class = "landable"
			out.Counts.Landable++
		}
		out.Changes = append(out.Changes, x.FrontierChange)
	}
	out.Counts.Total = len(out.Changes)
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}

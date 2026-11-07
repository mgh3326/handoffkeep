package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// OperatorDeskLane owns a project that has no design session. It is the one
// place the fallback is defined: every owner response carries both the stored
// design_lane and the resolved effective_owner, so no caller repeats the rule
// (decision/2026-11-07/design-session-desk-authority, hk 3420).
const OperatorDeskLane = "operator-desk"

// ProjectOwnerHistoryMax bounds one project's event history read.
const ProjectOwnerHistoryMax = 100

// ProjectOwner is one project's design-session ownership as the API reads it.
// DesignLane is the stored value ("" when nobody has set one); EffectiveOwner
// is DesignLane when set and OperatorDeskLane otherwise.
type ProjectOwner struct {
	Name           string     `json:"name"`
	DesignLane     string     `json:"design_lane"`
	EffectiveOwner string     `json:"effective_owner"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// ProjectOwnerEvent is one recorded change of a project's design lane. The
// empty lane is a real value: it means the owner was cleared.
type ProjectOwnerEvent struct {
	ID           int64     `json:"id"`
	Project      string    `json:"project"`
	PreviousLane string    `json:"previous_lane"`
	DesignLane   string    `json:"design_lane"`
	Actor        string    `json:"actor"`
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ProjectOwnerDetail is one project plus the recent ownership events.
type ProjectOwnerDetail struct {
	ProjectOwner
	Events []ProjectOwnerEvent `json:"events"`
}

func effectiveProjectOwner(designLane string) string {
	if designLane == "" {
		return OperatorDeskLane
	}
	return designLane
}

// projectOwner selects one owner row and the time it last changed. The lateral
// subquery is the latest event by id, so updated_at never regresses under equal
// timestamps.
const projectOwnerSelect = `SELECT p.name, p.design_lane, e.created_at
FROM task_projects p
LEFT JOIN LATERAL (SELECT created_at FROM project_owner_events WHERE project = p.name ORDER BY id DESC LIMIT 1) e ON true`

func scanProjectOwner(row pgx.Row) (ProjectOwner, error) {
	var owner ProjectOwner
	if err := row.Scan(&owner.Name, &owner.DesignLane, &owner.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectOwner{}, ErrProjectNotFound
		}
		return ProjectOwner{}, err
	}
	owner.EffectiveOwner = effectiveProjectOwner(owner.DesignLane)
	return owner, nil
}

// ListProjectOwners returns every configured project with its owner, in name
// order. A project nobody has claimed still appears, owned by operator-desk.
func (s *Store) ListProjectOwners(ctx context.Context) ([]ProjectOwner, error) {
	rows, err := s.pool.Query(ctx, projectOwnerSelect+` ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectOwner{}
	for rows.Next() {
		owner, err := scanProjectOwner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

// GetProjectOwner returns one project and its current owner. A malformed name
// is a validation error like GetProjectMetadata's; only a well-formed name the
// vocabulary does not hold is ErrProjectNotFound.
func (s *Store) GetProjectOwner(ctx context.Context, project string) (ProjectOwner, error) {
	if !validName(project) {
		return ProjectOwner{}, errors.New("invalid project")
	}
	return scanProjectOwner(s.pool.QueryRow(ctx, projectOwnerSelect+` WHERE p.name=$1`, project))
}

// SetProjectDesignLane records which design session owns a project. It locks
// the project row, updates the column and appends one event in the same
// transaction. An empty lane clears the owner. Setting the lane it already has
// is a no-op and writes no event. An unknown project is ErrProjectNotFound; a
// malformed lane is a validation error.
func (s *Store) SetProjectDesignLane(ctx context.Context, project, lane, actor, note string) (ProjectOwner, error) {
	if !validName(project) || (lane != "" && !validName(lane)) || actor == "" || !validText(actor, 128) || !validText(note, MaxBytes) {
		return ProjectOwner{}, errors.New("invalid project design lane change")
	}
	if err := guard.Reject(note); err != nil {
		return ProjectOwner{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectOwner{}, err
	}
	defer tx.Rollback(ctx)
	var previous string
	// FOR NO KEY UPDATE is the task_projects row lock this package already
	// uses (CreateStatusUpdate): it serializes the project writers without
	// conflicting with the KEY SHARE a task or milestone insert takes through
	// the foreign key (lockorder.go, exceptions).
	if err = tx.QueryRow(ctx, `SELECT design_lane FROM task_projects WHERE name=$1 FOR NO KEY UPDATE`, project).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectOwner{}, ErrProjectNotFound
		}
		return ProjectOwner{}, err
	}
	if previous == lane {
		owner, err := scanProjectOwner(tx.QueryRow(ctx, projectOwnerSelect+` WHERE p.name=$1`, project))
		if err != nil {
			return ProjectOwner{}, err
		}
		return owner, tx.Commit(ctx)
	}
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `UPDATE task_projects SET design_lane=$2 WHERE name=$1`, project, lane); err != nil {
		return ProjectOwner{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_owner_events(project,previous_lane,design_lane,actor,note,created_at) VALUES($1,$2,$3,$4,$5,$6)`, project, previous, lane, actor, note, now); err != nil {
		return ProjectOwner{}, err
	}
	owner := ProjectOwner{Name: project, DesignLane: lane, EffectiveOwner: effectiveProjectOwner(lane), UpdatedAt: &now}
	return owner, tx.Commit(ctx)
}

// ProjectOwnerHistory returns a project's most recent ownership events,
// newest first. Its ORDER BY ends in id so pages are deterministic.
func (s *Store) ProjectOwnerHistory(ctx context.Context, project string, limit int) ([]ProjectOwnerEvent, error) {
	if !validName(project) {
		return nil, errors.New("invalid project")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > ProjectOwnerHistoryMax {
		limit = ProjectOwnerHistoryMax
	}
	rows, err := s.pool.Query(ctx, `SELECT id,project,previous_lane,design_lane,actor,note,created_at FROM project_owner_events WHERE project=$1 ORDER BY id DESC LIMIT $2`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectOwnerEvent{}
	for rows.Next() {
		var e ProjectOwnerEvent
		if err := rows.Scan(&e.ID, &e.Project, &e.PreviousLane, &e.DesignLane, &e.Actor, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

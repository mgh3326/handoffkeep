package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrMilestoneNotFound = errors.New("milestone_not_found")
	ErrMilestoneProject  = errors.New("milestone_project")
)

type ProjectMilestone struct {
	ID          int64      `json:"id"`
	Project     string     `json:"project"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	TargetAt    *time.Time `json:"target_at"`
	Status      string     `json:"status"`
	SortOrder   int        `json:"sort_order"`
}

// SaveMilestone creates or updates a milestone. Project is immutable after creation.
func (s *Store) SaveMilestone(ctx context.Context, m ProjectMilestone) (ProjectMilestone, error) {
	if m.ID < 0 || !validName(m.Project) || m.Name == "" || !validText(m.Name, 256) || !validText(m.Description, MaxBytes) || !validText(m.Status, 128) {
		return ProjectMilestone{}, errors.New("invalid milestone")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectMilestone{}, err
	}
	defer tx.Rollback(ctx)
	var known bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, m.Project).Scan(&known); err != nil {
		return ProjectMilestone{}, err
	}
	if !known {
		return ProjectMilestone{}, ErrProjectNotFound
	}
	if m.ID == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO project_milestones(project,name,description,target_at,status,sort_order) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, m.Project, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder).Scan(&m.ID)
	} else {
		tag, e := tx.Exec(ctx, `UPDATE project_milestones SET name=$3,description=$4,target_at=$5,status=$6,sort_order=$7 WHERE id=$1 AND project=$2`, m.ID, m.Project, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder)
		err = e
		if err == nil && tag.RowsAffected() == 0 {
			return ProjectMilestone{}, ErrMilestoneNotFound
		}
	}
	if err != nil {
		return ProjectMilestone{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *Store) SetTaskMilestone(ctx context.Context, id int64, milestone *int64, by string) (Task, bool, error) {
	if id < 1 || (milestone != nil && *milestone < 1) || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid task milestone")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if err = s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	if milestone != nil {
		var project string
		if err = tx.QueryRow(ctx, `SELECT project FROM project_milestones WHERE id=$1`, *milestone).Scan(&project); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Task{}, false, ErrMilestoneNotFound
			}
			return Task{}, false, err
		}
		if x.Project == nil || *x.Project != project {
			return Task{}, false, ErrMilestoneProject
		}
	}
	from, to := "", ""
	if x.MilestoneID != nil {
		from = fmt.Sprint(*x.MilestoneID)
	}
	if milestone != nil {
		to = fmt.Sprint(*milestone)
	}
	if from == to {
		return x, false, tx.Commit(ctx)
	}
	now := time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET milestone_id=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, milestone, now), &x); err != nil {
		return Task{}, false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, Into: &x, At: now, Field: "milestone_id", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

func (s *Store) GetMilestone(ctx context.Context, id int64) (ProjectMilestone, error) {
	if id < 1 {
		return ProjectMilestone{}, errors.New("invalid milestone")
	}
	var m ProjectMilestone
	err := s.pool.QueryRow(ctx, `SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=$1`, id).Scan(&m.ID, &m.Project, &m.Name, &m.Description, &m.TargetAt, &m.Status, &m.SortOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectMilestone{}, ErrMilestoneNotFound
	}
	return m, err
}

type MilestonePatch struct {
	Project     *string    `json:"project,omitempty"`
	Name        *string    `json:"name,omitempty"`
	Description *string    `json:"description,omitempty"`
	Status      *string    `json:"status,omitempty"`
	SortOrder   *int       `json:"sort_order,omitempty"`
	SetTarget   bool       `json:"set_target"`
	TargetAt    *time.Time `json:"target_at,omitempty"`
}

func (p *MilestonePatch) UnmarshalJSON(data []byte) error {
	type patchAlias MilestonePatch
	var decoded patchAlias
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, hasTarget := fields["target_at"]
	if hasTarget && !decoded.SetTarget {
		return errors.New("target_at requires set_target")
	}
	// set_target without target_at would clear the date silently; a clear is
	// an explicit "target_at": null.
	if decoded.SetTarget && !hasTarget {
		return errors.New("set_target requires target_at")
	}
	// null is not "unchanged" for these fields: refuse it so a client that
	// means to clear one gets an error instead of a silent no-op.
	for _, key := range []string{"project", "name", "description", "status", "sort_order"} {
		if raw, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must not be null", key)
		}
	}
	*p = MilestonePatch(decoded)
	return nil
}

// MarshalJSON sends "target_at": null when SetTarget clears the date, which
// the omitempty tag would otherwise drop.
func (p MilestonePatch) MarshalJSON() ([]byte, error) {
	type patchAlias MilestonePatch
	if !p.SetTarget || p.TargetAt != nil {
		return json.Marshal(patchAlias(p))
	}
	encoded, err := json.Marshal(patchAlias(p))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	fields["target_at"] = json.RawMessage("null")
	return json.Marshal(fields)
}

func (s *Store) PatchMilestone(ctx context.Context, id int64, p MilestonePatch) (ProjectMilestone, error) {
	if id < 1 || (!p.SetTarget && p.TargetAt != nil) || (p.Project != nil && !validName(*p.Project)) || (p.Name != nil && (*p.Name == "" || !validText(*p.Name, 256))) || (p.Description != nil && !validText(*p.Description, MaxBytes)) || (p.Status != nil && !validText(*p.Status, 128)) {
		return ProjectMilestone{}, errors.New("invalid milestone")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectMilestone{}, err
	}
	defer tx.Rollback(ctx)
	var m ProjectMilestone
	err = tx.QueryRow(ctx, `SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=$1 FOR UPDATE`, id).Scan(&m.ID, &m.Project, &m.Name, &m.Description, &m.TargetAt, &m.Status, &m.SortOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectMilestone{}, ErrMilestoneNotFound
	}
	if err != nil {
		return ProjectMilestone{}, err
	}
	if p.Project != nil && *p.Project != m.Project {
		return ProjectMilestone{}, ErrMilestoneProject
	}
	if p.Name != nil {
		m.Name = *p.Name
	}
	if p.Description != nil {
		m.Description = *p.Description
	}
	if p.Status != nil {
		m.Status = *p.Status
	}
	if p.SortOrder != nil {
		m.SortOrder = *p.SortOrder
	}
	if p.SetTarget {
		m.TargetAt = p.TargetAt
	}
	if _, err = tx.Exec(ctx, `UPDATE project_milestones SET name=$2,description=$3,target_at=$4,status=$5,sort_order=$6 WHERE id=$1`, id, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder); err != nil {
		return ProjectMilestone{}, err
	}
	return m, tx.Commit(ctx)
}

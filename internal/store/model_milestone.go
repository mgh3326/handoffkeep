package store

import (
	"context"
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
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, At: now, Field: "milestone_id", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

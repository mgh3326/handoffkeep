package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	in := m
	err := s.retryTx(ctx, func() error {
		m = in
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		var known bool
		if err = tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=$1)`, `SELECT EXISTS(SELECT 1 FROM task_projects WHERE name=?)`), m.Project).Scan(&known); err != nil {
			return err
		}
		if !known {
			return ErrProjectNotFound
		}
		if m.ID == 0 {
			m.ID, err = tx.InsertID(ctx, s.q(`INSERT INTO project_milestones(project,name,description,target_at,status,sort_order) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, `INSERT INTO project_milestones(project,name,description,target_at,status,sort_order) VALUES(?,?,?,?,?,?)`), m.Project, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder)
		} else {
			var n int64
			if s.dialect == mysqlDialect {
				n, err = tx.Exec(ctx, `UPDATE project_milestones SET name=?,description=?,target_at=?,status=?,sort_order=? WHERE id=? AND project=?`, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder, m.ID, m.Project)
			} else {
				n, err = tx.Exec(ctx, `UPDATE project_milestones SET name=$3,description=$4,target_at=$5,status=$6,sort_order=$7 WHERE id=$1 AND project=$2`, m.ID, m.Project, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder)
			}
			if err == nil && n == 0 {
				return ErrMilestoneNotFound
			}
		}
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return ProjectMilestone{}, err
	}
	return m, nil
}

func (s *Store) SetTaskMilestone(ctx context.Context, id int64, milestone *int64, by string) (Task, bool, error) {
	if id < 1 || (milestone != nil && *milestone < 1) || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid task milestone")
	}
	var x Task
	changed := false
	err := s.retryTx(ctx, func() error {
		x, changed = Task{}, false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), id), &x); err != nil {
			if errors.Is(err, ErrNoRows) {
				return ErrTaskNotFound
			}
			return err
		}
		if milestone != nil {
			var project string
			if err = tx.QueryRow(ctx, s.q(`SELECT project FROM project_milestones WHERE id=$1`, `SELECT project FROM project_milestones WHERE id=?`), *milestone).Scan(&project); err != nil {
				if errors.Is(err, ErrNoRows) {
					return ErrMilestoneNotFound
				}
				return err
			}
			if x.Project == nil || *x.Project != project {
				return ErrMilestoneProject
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
			return tx.Commit(ctx)
		}
		now := time.Now().UTC()
		if err = s.updateTaskReturningCore(ctx, tx, &x, id,
			`UPDATE tasks SET milestone_id=$2,updated_at=$3 WHERE id=$1 RETURNING `, []any{id, milestone, now},
			`UPDATE tasks SET milestone_id=?,updated_at=? WHERE id=?`, []any{milestone, now, id}); err != nil {
			return err
		}
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, Into: &x, At: now, Field: "milestone_id", FromValue: from, ToValue: to}); err != nil {
			return err
		}
		changed = true
		return tx.Commit(ctx)
	})
	if err != nil {
		return Task{}, false, err
	}
	return x, changed, nil
}

func (s *Store) GetMilestone(ctx context.Context, id int64) (ProjectMilestone, error) {
	if id < 1 {
		return ProjectMilestone{}, errors.New("invalid milestone")
	}
	var m ProjectMilestone
	err := s.db().QueryRow(ctx, s.q(`SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=$1`, `SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=?`), id).Scan(&m.ID, &m.Project, &m.Name, &m.Description, &m.TargetAt, &m.Status, &m.SortOrder)
	if errors.Is(err, ErrNoRows) {
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
	var m ProjectMilestone
	err := s.retryTx(ctx, func() error {
		m = ProjectMilestone{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		err = tx.QueryRow(ctx, s.q(`SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=$1 FOR UPDATE`, `SELECT id,project,name,description,target_at,status,sort_order FROM project_milestones WHERE id=? FOR UPDATE`), id).Scan(&m.ID, &m.Project, &m.Name, &m.Description, &m.TargetAt, &m.Status, &m.SortOrder)
		if errors.Is(err, ErrNoRows) {
			return ErrMilestoneNotFound
		}
		if err != nil {
			return err
		}
		if p.Project != nil && *p.Project != m.Project {
			return ErrMilestoneProject
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
		if _, err = tx.Exec(ctx, s.q(`UPDATE project_milestones SET name=$2,description=$3,target_at=$4,status=$5,sort_order=$6 WHERE id=$1`, `UPDATE project_milestones SET name=?,description=?,target_at=?,status=?,sort_order=? WHERE id=?`), milestoneUpdateArgsCore(s.dialect, id, m)...); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return ProjectMilestone{}, err
	}
	return m, nil
}

// milestoneUpdateArgsCore orders the arguments of PatchMilestone's UPDATE: the
// id is $1 on PostgreSQL and the last placeholder on MySQL.
func milestoneUpdateArgsCore(d storeDialect, id int64, m ProjectMilestone) []any {
	if d == mysqlDialect {
		return []any{m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder, id}
	}
	return []any{id, m.Name, m.Description, m.TargetAt, m.Status, m.SortOrder}
}

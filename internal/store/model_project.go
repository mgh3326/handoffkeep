package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrProjectNotFound = errors.New("project_not_found")

type ProjectMetadata struct {
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	StartAt  *time.Time `json:"start_at"`
	TargetAt *time.Time `json:"target_at"`
}

func (s *Store) SetProjectMetadata(ctx context.Context, p ProjectMetadata) (ProjectMetadata, error) {
	if !validName(p.Name) || !validText(p.Status, 128) || (p.StartAt != nil && p.TargetAt != nil && p.TargetAt.Before(*p.StartAt)) {
		return ProjectMetadata{}, errors.New("invalid project metadata")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE task_projects SET status=$2,start_at=$3,target_at=$4 WHERE name=$1`, p.Name, p.Status, p.StartAt, p.TargetAt)
	if err != nil {
		return ProjectMetadata{}, err
	}
	if tag.RowsAffected() == 0 {
		return ProjectMetadata{}, ErrProjectNotFound
	}
	return p, nil
}

func (s *Store) GetProjectMetadata(ctx context.Context, name string) (ProjectMetadata, error) {
	if !validName(name) {
		return ProjectMetadata{}, errors.New("invalid project")
	}
	var p ProjectMetadata
	err := s.pool.QueryRow(ctx, `SELECT name,status,start_at,target_at FROM task_projects WHERE name=$1`, name).Scan(&p.Name, &p.Status, &p.StartAt, &p.TargetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectMetadata{}, ErrProjectNotFound
	}
	if err != nil {
		return ProjectMetadata{}, err
	}
	return p, nil
}

// ProjectMetadataPatch distinguishes an omitted date from a request to clear it.
type ProjectMetadataPatch struct {
	Status    *string    `json:"status"`
	SetStart  bool       `json:"set_start"`
	StartAt   *time.Time `json:"start_at"`
	SetTarget bool       `json:"set_target"`
	TargetAt  *time.Time `json:"target_at"`
}

func (s *Store) PatchProjectMetadata(ctx context.Context, name string, patch ProjectMetadataPatch) (ProjectMetadata, error) {
	if !validName(name) || (patch.Status != nil && !validText(*patch.Status, 128)) {
		return ProjectMetadata{}, errors.New("invalid project metadata")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectMetadata{}, err
	}
	defer tx.Rollback(ctx)
	var p ProjectMetadata
	err = tx.QueryRow(ctx, `SELECT name,status,start_at,target_at FROM task_projects WHERE name=$1 FOR UPDATE`, name).Scan(&p.Name, &p.Status, &p.StartAt, &p.TargetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectMetadata{}, ErrProjectNotFound
	}
	if err != nil {
		return ProjectMetadata{}, err
	}
	if patch.Status != nil {
		p.Status = *patch.Status
	}
	if patch.SetStart {
		p.StartAt = patch.StartAt
	}
	if patch.SetTarget {
		p.TargetAt = patch.TargetAt
	}
	if p.StartAt != nil && p.TargetAt != nil && p.TargetAt.Before(*p.StartAt) {
		return ProjectMetadata{}, errors.New("invalid project metadata")
	}
	if _, err = tx.Exec(ctx, `UPDATE task_projects SET status=$2,start_at=$3,target_at=$4 WHERE name=$1`, name, p.Status, p.StartAt, p.TargetAt); err != nil {
		return ProjectMetadata{}, err
	}
	return p, tx.Commit(ctx)
}

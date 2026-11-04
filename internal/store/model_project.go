package store

import (
	"bytes"
	"context"
	"encoding/json"
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
	StartAt   *time.Time `json:"start_at,omitempty"`
	SetTarget bool       `json:"set_target"`
	TargetAt  *time.Time `json:"target_at,omitempty"`
}

func (p *ProjectMetadataPatch) UnmarshalJSON(data []byte) error {
	type patchAlias ProjectMetadataPatch
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
	if _, ok := fields["start_at"]; ok && !decoded.SetStart {
		return errors.New("start_at requires set_start")
	}
	if _, ok := fields["target_at"]; ok && !decoded.SetTarget {
		return errors.New("target_at requires set_target")
	}
	*p = ProjectMetadataPatch(decoded)
	return nil
}

func (s *Store) PatchProjectMetadata(ctx context.Context, name string, patch ProjectMetadataPatch) (ProjectMetadata, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectMetadata{}, err
	}
	defer tx.Rollback(ctx)
	p, err := patchProjectMetadataTx(ctx, tx, name, patch)
	if err != nil {
		return ProjectMetadata{}, err
	}
	return p, tx.Commit(ctx)
}

func patchProjectMetadataTx(ctx context.Context, tx pgx.Tx, name string, patch ProjectMetadataPatch) (ProjectMetadata, error) {
	if !validName(name) || (patch.Status != nil && !validText(*patch.Status, 128)) || (!patch.SetStart && patch.StartAt != nil) || (!patch.SetTarget && patch.TargetAt != nil) {
		return ProjectMetadata{}, errors.New("invalid project metadata")
	}
	var p ProjectMetadata
	err := tx.QueryRow(ctx, `SELECT name,status,start_at,target_at FROM task_projects WHERE name=$1 FOR UPDATE`, name).Scan(&p.Name, &p.Status, &p.StartAt, &p.TargetAt)
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
	return p, nil
}

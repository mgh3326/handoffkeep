package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type LinearMetadata struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
	Deleted   bool      `json:"deleted"`
}

var ErrLinearBoardLabelUnavailable = errors.New("linear_board_label_unavailable")

func applyLinearMetadata(ctx context.Context, tx pgx.Tx, items []LinearMetadata) error {
	for _, m := range items {
		if m.ID == "" || (m.Kind != "team" && m.Kind != "state" && m.Kind != "label") || m.UpdatedAt.IsZero() {
			return errors.New("invalid linear metadata")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO linear_metadata(kind,id,team_id,name,updated_at,deleted) VALUES($1,$2,$3,$4,$5,$6)
  ON CONFLICT(kind,id) DO UPDATE SET team_id=EXCLUDED.team_id,name=EXCLUDED.name,updated_at=EXCLUDED.updated_at,deleted=EXCLUDED.deleted
  WHERE linear_metadata.updated_at<EXCLUDED.updated_at`, m.Kind, m.ID, m.TeamID, m.Name, m.UpdatedAt, m.Deleted); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ApplyLinearMetadata(ctx context.Context, items []LinearMetadata) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = applyLinearMetadata(ctx, tx, items); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Initialization is durable and serialized across processes. Subsequent
// deliveries read hk only; metadata changes arrive through signed webhooks.
func (s *Store) EnsureLinearBoardMetadata(ctx context.Context, fetch func(context.Context) ([]LinearMetadata, error)) error {
	return s.refreshLinearBoardMetadata(ctx, false, fetch)
}
func (s *Store) RefreshLinearBoardMetadata(ctx context.Context, fetch func(context.Context) ([]LinearMetadata, error)) error {
	return s.refreshLinearBoardMetadata(ctx, true, fetch)
}
func (s *Store) refreshLinearBoardMetadata(ctx context.Context, force bool, fetch func(context.Context) ([]LinearMetadata, error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var ready bool
	if err = tx.QueryRow(ctx, `SELECT initialized FROM linear_metadata_state WHERE singleton FOR UPDATE`).Scan(&ready); err != nil {
		return err
	}
	if ready && !force {
		return tx.Commit(ctx)
	}
	items, err := fetch(ctx)
	if err != nil {
		return err
	}
	if err = applyLinearMetadata(ctx, tx, items); err != nil {
		return err
	}
	if force {
		if _, err = tx.Exec(ctx, `DELETE FROM linear_label_failures`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE linear_metadata_state SET initialized=true WHERE singleton`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) LinearBoardMetadata(ctx context.Context) ([]LinearMetadata, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind,id,team_id,name,updated_at,deleted FROM linear_metadata WHERE NOT deleted ORDER BY kind,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []LinearMetadata{}
	for rows.Next() {
		var m LinearMetadata
		if err = rows.Scan(&m.Kind, &m.ID, &m.TeamID, &m.Name, &m.UpdatedAt, &m.Deleted); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}
func (s *Store) ResolveLinearBoardLabel(ctx context.Context, team, name string, create func(context.Context) (LinearMetadata, error)) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// One label creation attempt, even with multiple workers. Failures are durable
	// configuration faults; a later label webhook supplies the missing entity.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180177)`); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM linear_metadata WHERE kind='label' AND name=$1 AND team_id IN ('',$2) AND NOT deleted ORDER BY (team_id=$2) DESC,id LIMIT 1`, name, team).Scan(&id)
	if err == nil {
		return id, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var failed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_label_failures WHERE team_id=$1 AND name=$2)`, team, name).Scan(&failed); err != nil {
		return "", err
	}
	if failed {
		return "", ErrLinearBoardLabelUnavailable
	}
	m, err := create(ctx)
	if err != nil {
		if errors.Is(err, ErrLinearBoardLabelUnavailable) {
			if _, saveErr := tx.Exec(ctx, `INSERT INTO linear_label_failures(team_id,name) VALUES($1,$2) ON CONFLICT DO NOTHING`, team, name); saveErr != nil {
				return "", saveErr
			}
			if saveErr := tx.Commit(ctx); saveErr != nil {
				return "", saveErr
			}
		}
		return "", err
	}
	if m.Kind != "label" || m.Name != name || m.TeamID != team || m.Deleted {
		return "", errors.New("invalid created linear label")
	}
	if err = applyLinearMetadata(ctx, tx, []LinearMetadata{m}); err != nil {
		return "", err
	}
	return m.ID, tx.Commit(ctx)
}

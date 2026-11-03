package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

const linearLabelLookup = `SELECT id FROM linear_metadata WHERE kind='label' AND name=$1 AND team_id IN ('',$2) AND NOT deleted ORDER BY (team_id=$2) DESC,id LIMIT 1`

var ErrLinearLabelPending = errors.New("linear_label_creation_pending")

func (s *Store) LinearBoardLabelAvailable(ctx context.Context, team, name string) (bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, linearLabelLookup, name, team).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (s *Store) ResolveLinearBoardLabel(ctx context.Context, team, name string, create func(context.Context) (LinearMetadata, error)) (string, error) {
	// Hits never take an advisory lock or wait for another label's HTTP call.
	var id string
	err := s.pool.QueryRow(ctx, linearLabelLookup, name, team).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Serialize only the short claim, per team/name, and re-check after locking.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,824180177))`, team+":"+name); err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, linearLabelLookup, name, team).Scan(&id)
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
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(nonce[:])
	tag, err := tx.Exec(ctx, `INSERT INTO linear_label_attempts(team_id,name,token,started_at) VALUES($1,$2,$3,clock_timestamp())
 ON CONFLICT(team_id,name) DO UPDATE SET token=EXCLUDED.token,started_at=EXCLUDED.started_at
 WHERE linear_label_attempts.started_at<clock_timestamp()-interval '2 minutes'`, team, name, token)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrLinearLabelPending
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	// No DB lock or pooled connection is held across remote create/readback.
	m, createErr := create(ctx)
	finish, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer finish.Rollback(ctx)
	tag, err = finish.Exec(ctx, `DELETE FROM linear_label_attempts WHERE team_id=$1 AND name=$2 AND token=$3`, team, name, token)
	if err != nil {
		return "", err
	}
	// A matching webhook may have won the race while HTTP was in flight.
	// Prefer current stored metadata to persisting an obsolete refusal.
	var currentID string
	lookupErr := finish.QueryRow(ctx, linearLabelLookup, name, team).Scan(&currentID)
	if lookupErr == nil {
		return currentID, finish.Commit(ctx)
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return "", lookupErr
	}
	if tag.RowsAffected() == 0 {
		return "", ErrLinearLabelPending
	}
	if createErr != nil {
		if errors.Is(createErr, ErrLinearBoardLabelUnavailable) {
			if _, err = finish.Exec(ctx, `INSERT INTO linear_label_failures(team_id,name) VALUES($1,$2) ON CONFLICT DO NOTHING`, team, name); err != nil {
				return "", err
			}
		}
		if err = finish.Commit(ctx); err != nil {
			return "", err
		}
		return "", createErr
	}
	if m.Kind != "label" || m.ID == "" || m.Name != name || m.TeamID != team || m.Deleted {
		return "", errors.New("invalid created linear label")
	}
	if err = applyLinearMetadata(ctx, finish, []LinearMetadata{m}); err != nil {
		return "", err
	}
	// Do not return a stale created id if a newer webhook tombstoned it.
	err = finish.QueryRow(ctx, linearLabelLookup, name, team).Scan(&currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = finish.Commit(ctx); err != nil {
			return "", err
		}
		return "", ErrLinearLabelPending
	}
	if err != nil {
		return "", err
	}
	return currentID, finish.Commit(ctx)
}

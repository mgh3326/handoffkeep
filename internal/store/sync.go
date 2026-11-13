package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const SyncLimit = 1000

var ErrInvalidSync = errors.New("invalid_sync")

type SyncChange struct {
	SyncID  int64     `json:"sync_id"`
	Model   string    `json:"model"`
	ModelID string    `json:"model_id"`
	Action  string    `json:"action"`
	At      time.Time `json:"at"`
}
type SyncMetadata struct {
	LastSyncID    int64 `json:"last_sync_id"`
	MaxSyncID     int64 `json:"max_sync_id"`
	SchemaVersion int   `json:"schema_version"`
}
type SyncReset struct{ MinSyncID int64 }

func (e *SyncReset) Error() string { return "sync reset required" }

func (s *Store) SyncDelta(ctx context.Context, after int64, limit int) ([]SyncChange, SyncMetadata, error) {
	meta := SyncMetadata{LastSyncID: after, SchemaVersion: SchemaVersion}
	if after < 0 || limit < 1 || limit > SyncLimit {
		return nil, meta, ErrInvalidSync
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, meta, err
	}
	defer tx.Rollback(ctx)
	changes, meta, err := syncDeltaTx(ctx, tx, after, limit)
	if err != nil {
		return nil, meta, err
	}
	return changes, meta, tx.Commit(ctx)
}

// syncDeltaTx reads one delta page in the caller's snapshot. The desk ledger
// reads its "changed since last ack" section through it, so the ledger and
// the feed share one reset and watermark rule.
func syncDeltaTx(ctx context.Context, tx pgx.Tx, after int64, limit int) ([]SyncChange, SyncMetadata, error) {
	meta := SyncMetadata{LastSyncID: after, SchemaVersion: SchemaVersion}
	if after < 0 || limit < 1 || limit > SyncLimit {
		return nil, meta, ErrInvalidSync
	}
	var floor int64
	if err := tx.QueryRow(ctx, `SELECT max_sync_id,pruned_through FROM sync_state WHERE singleton`).Scan(&meta.MaxSyncID, &floor); err != nil {
		return nil, meta, err
	}
	if after < floor {
		minID := meta.MaxSyncID + 1
		if err := tx.QueryRow(ctx, `SELECT COALESCE(min(sync_id),$1) FROM sync_log`, minID).Scan(&minID); err != nil {
			return nil, meta, err
		}
		return nil, meta, &SyncReset{MinSyncID: minID}
	}
	if after > meta.MaxSyncID {
		return nil, meta, ErrInvalidSync
	}
	rows, err := tx.Query(ctx, `SELECT sync_id,model,model_id,action,at FROM sync_log WHERE sync_id IS NOT NULL AND sync_id>$1 ORDER BY sync_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, meta, err
	}
	defer rows.Close()
	changes := make([]SyncChange, 0)
	for rows.Next() {
		var c SyncChange
		if err = rows.Scan(&c.SyncID, &c.Model, &c.ModelID, &c.Action, &c.At); err != nil {
			return nil, meta, err
		}
		changes = append(changes, c)
		meta.LastSyncID = c.SyncID
	}
	return changes, meta, rows.Err()
}

// PruneSync removes a bounded prefix, retaining a durable reset boundary even
// when the log becomes empty. Pending rows have no assigned ID and are never
// eligible for pruning.
func (s *Store) PruneSync(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("%w: retention must be positive", ErrInvalidSync)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT 1 FROM sync_state WHERE singleton FOR UPDATE`); err != nil {
		return 0, err
	}
	var through int64
	// Stop at the first recent entry, even if wall time moved backwards.
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(sync_id),0) FROM (SELECT sync_id FROM sync_log WHERE sync_id<COALESCE((SELECT min(sync_id) FROM sync_log WHERE at >= $1),9223372036854775807) ORDER BY sync_id LIMIT 10000) old`, time.Now().UTC().Add(-retention)).Scan(&through)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sync_log WHERE sync_id<=$1`, through)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_state SET pruned_through=GREATEST(pruned_through,$1) WHERE singleton`, through); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

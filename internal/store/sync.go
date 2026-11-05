package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

type SyncRow struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`
	Key       string    `json:"key,omitempty"`
}
type SyncBootstrap struct {
	Rows          []SyncRow `json:"rows"`
	Cursor        string    `json:"cursor,omitempty"`
	MaxSyncID     int64     `json:"max_sync_id"`
	SchemaVersion int       `json:"schema_version"`
}
type syncCursor struct {
	Model   string
	After   string
	Max     int64
	Version int
}

var syncSources = map[string]string{
	"document":     `SELECT id::text,updated_at,key FROM documents`,
	"task":         `SELECT id::text,updated_at,'' AS key FROM tasks`,
	"task_comment": `SELECT id::text,created_at AS updated_at,'' AS key FROM task_comments`,
	"decision":     `SELECT id,updated_at,'' AS key FROM sync_decisions`,
}

func (s *Store) SyncBootstrap(ctx context.Context, model, cursor string, limit int) (SyncBootstrap, error) {
	out := SyncBootstrap{Rows: make([]SyncRow, 0), SchemaVersion: SchemaVersion}
	source, ok := syncSources[model]
	if !ok || limit < 1 || limit > SyncLimit {
		return out, ErrInvalidSync
	}
	cur := syncCursor{Model: model, Version: SchemaVersion}
	if cursor != "" {
		if len(cursor) > 4096 {
			return out, ErrInvalidSync
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(b, &cur) != nil || cur.Model != model || cur.Version != SchemaVersion || cur.After == "" || cur.Max < 0 {
			return out, ErrInvalidSync
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var maxID, floor int64
	if err = tx.QueryRow(ctx, `SELECT max_sync_id,pruned_through FROM sync_state WHERE singleton`).Scan(&maxID, &floor); err != nil {
		return out, err
	}
	if cursor == "" {
		cur.Max = maxID
	}
	if cur.Max > maxID {
		return out, ErrInvalidSync
	}
	if cur.Max < floor {
		minID := maxID + 1
		if err = tx.QueryRow(ctx, `SELECT COALESCE(min(sync_id),$1) FROM sync_log`, minID).Scan(&minID); err != nil {
			return out, err
		}
		return out, &SyncReset{MinSyncID: minID}
	}
	out.MaxSyncID = cur.Max
	// IDs are compared as text consistently; no timestamp cursor can skip a
	// row that is updated while a client is paging its bootstrap.
	rows, err := tx.Query(ctx, `SELECT id,updated_at,key FROM (`+source+`) source WHERE id COLLATE "C">$1 ORDER BY id COLLATE "C" LIMIT $2`, cur.After, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var row SyncRow
		if err = rows.Scan(&row.ID, &row.UpdatedAt, &row.Key); err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
		cur.After = out.Rows[limit-1].ID
		b, _ := json.Marshal(cur)
		out.Cursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, tx.Commit(ctx)
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

// GetSyncRow returns the current raw model row for a delta model_id. Decisions
// expose the complete request (including question and resolution) and options.
func (s *Store) GetSyncRow(ctx context.Context, model, id string) (json.RawMessage, bool, error) {
	if id == "" || !validText(id, 512) {
		return nil, false, ErrInvalidSync
	}
	var query string
	var arg any = id
	switch model {
	case "document", "task", "task_comment":
		number, err := strconv.ParseInt(id, 10, 64)
		if err != nil || number < 1 {
			return nil, false, ErrInvalidSync
		}
		arg = number
		table := map[string]string{"document": "documents", "task": "tasks", "task_comment": "task_comments"}[model]
		// model_row.* is the whole row even where the table has a column named
		// like the alias (tasks.source).
		query = `SELECT to_jsonb(model_row.*) FROM ` + table + ` model_row WHERE id=$1`
	case "decision":
		query = `SELECT (t.refs->'decision_request') || jsonb_build_object('task_id',t.id,'updated_at',d.updated_at,'options',t.refs->'decision_options') FROM sync_decisions d JOIN tasks t ON t.id=d.task_id WHERE d.id=$1`
	default:
		return nil, false, ErrInvalidSync
	}
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx, query, arg).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

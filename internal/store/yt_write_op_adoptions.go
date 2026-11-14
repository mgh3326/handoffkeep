package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// RecordAdoption is ytwrite.Ledger's adoption record (hk 4570,
// yt_write_op_adoptions): in one transaction, the op row FOR SHARE (so no
// state move commits between the check and the insert), then the new row.
// It takes no lane lock and no other lock (lockorder.go).
func (l *YTWriteLedger) RecordAdoption(ctx context.Context, now time.Time, id int64, attempts int, ytID string) error {
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	var have int
	err = tx.QueryRow(ctx, `SELECT state, attempts FROM yt_write_ops WHERE id=$1 FOR SHARE`, id).Scan(&state, &have)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.ErrNotFound
	}
	if err != nil {
		return err
	}
	if a, found, err := readYTAdoption(ctx, tx, id); err != nil {
		return err
	} else if found {
		if a.Attempts != attempts || a.YTID != ytID {
			return fmt.Errorf("%w: op %d already recorded adopting %s in attempt %d", ytwrite.ErrConflictingResult, id, a.YTID, a.Attempts)
		}
		return nil
	}
	if ytwrite.State(state) != ytwrite.StateDispatched || have != attempts {
		return fmt.Errorf("%w: record adoption of attempt %d from %s attempt %d", ytwrite.ErrState, attempts, state, have)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yt_write_op_adoptions(op_id, attempts, yt_id, recorded_at) VALUES ($1,$2,$3,$4)`,
		id, attempts, ytID, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Adoption reads op id's adoption record.
func (l *YTWriteLedger) Adoption(ctx context.Context, id int64) (ytwrite.Adoption, bool, error) {
	return readYTAdoption(ctx, l.s.pool, id)
}

func readYTAdoption(ctx context.Context, q ytRowQuerier, id int64) (ytwrite.Adoption, bool, error) {
	var a ytwrite.Adoption
	err := q.QueryRow(ctx, `SELECT attempts, yt_id FROM yt_write_op_adoptions WHERE op_id=$1`, id).Scan(&a.Attempts, &a.YTID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Adoption{}, false, nil
	}
	return a, err == nil, err
}

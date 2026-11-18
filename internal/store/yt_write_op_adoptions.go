package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// RecordAdoption is ytwrite.Ledger's adoption record (hk 4570,
// yt_write_op_adoptions): in one transaction, the op row FOR SHARE (so no
// state move commits between the check and the insert), then the new row.
// It takes no lane lock and no other lock (lockorder.go).
func (l *YTWriteLedger) RecordAdoption(ctx context.Context, now time.Time, id int64, attempts int, ytID string) error {
	return l.s.retryTx(ctx, func() error {
		return l.recordAdoptionOnce(ctx, now, id, attempts, ytID)
	})
}

// recordAdoptionOnce is one attempt of RecordAdoption's transaction.
func (l *YTWriteLedger) recordAdoptionOnce(ctx context.Context, now time.Time, id int64, attempts int, ytID string) error {
	tx, release, err := l.s.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback(ctx)
	var state string
	var have int
	err = tx.QueryRow(ctx, sqlFor(tx, `SELECT state, attempts FROM yt_write_ops WHERE id=$1 FOR SHARE`,
		`SELECT state, attempts FROM yt_write_ops WHERE id=? FOR SHARE`), id).Scan(&state, &have)
	if errors.Is(err, ErrNoRows) {
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
	if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_write_op_adoptions(op_id, attempts, yt_id, recorded_at) VALUES ($1,$2,$3,$4)`,
		`INSERT INTO yt_write_op_adoptions(op_id, attempts, yt_id, recorded_at) VALUES (?,?,?,?)`),
		id, attempts, ytID, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Adoption reads op id's adoption record.
func (l *YTWriteLedger) Adoption(ctx context.Context, id int64) (ytwrite.Adoption, bool, error) {
	return readYTAdoption(ctx, l.s.db(), id)
}

func readYTAdoption(ctx context.Context, q ytRowQuerier, id int64) (ytwrite.Adoption, bool, error) {
	var a ytwrite.Adoption
	err := q.QueryRow(ctx, sqlFor(q, `SELECT attempts, yt_id FROM yt_write_op_adoptions WHERE op_id=$1`,
		`SELECT attempts, yt_id FROM yt_write_op_adoptions WHERE op_id=?`), id).Scan(&a.Attempts, &a.YTID)
	if errors.Is(err, ErrNoRows) {
		return ytwrite.Adoption{}, false, nil
	}
	return a, err == nil, err
}

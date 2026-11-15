package store

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The issue-link sweep (hk 3841, design/2026-11-13/yt-issue-links §5.6, I13).
// The task_links consumer reads a change once and, with the mode off, ignores
// it; a link change made while off, or before the consumer's cursor existed,
// must still reach YouTrack when the mode is turned on. yt_links_sweep holds
// the one clock for that: generation counts the moments when changes went
// unhandled (an off-mode pass that read a row, the creation of the cursor),
// done_generation is the last generation a complete walk of the bound tasks
// finished at, and after_task_id is the walk's position. A walk is complete
// for generation g only if every slice of it ran at g: bumping the generation
// moves the position back to 0 in the same statement, and every slice's
// advance is fenced on (generation, position), so a slice that raced a bump
// is discarded and the walk starts again. A walk that reaches the end at the
// current generation is therefore a walk that began after the last bump,
// which is why no separate "walk started at" value is stored.

// YTLinksSweepPerPass bounds the bound tasks one sweep slice visits.
const YTLinksSweepPerPass = 200

// YTLinksSweepState is the singleton row of yt_links_sweep; no row is the
// zero state, which is complete (nothing was left unhandled).
type YTLinksSweepState struct {
	Generation     int64
	DoneGeneration int64
	AfterTaskID    int64
}

// Complete reports whether a walk finished at the current generation.
func (st YTLinksSweepState) Complete() bool { return st.DoneGeneration >= st.Generation }

// ytLinksSweepStateTx reads the sweep row without a lock.
func ytLinksSweepStateTx(ctx context.Context, q ytRowQuerier) (YTLinksSweepState, error) {
	var st YTLinksSweepState
	err := q.QueryRow(ctx, `SELECT generation, done_generation, after_task_id FROM yt_links_sweep WHERE singleton`).
		Scan(&st.Generation, &st.DoneGeneration, &st.AfterTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTLinksSweepState{}, nil
	}
	return st, err
}

// bumpYTLinksSweepTx moves the generation up by one and the walk back to its
// start, creating the row on the first call. One statement, so a slice that
// holds the row's earlier value cannot advance past it (the fence in
// RunYTLinksSweep). now is the caller's Go time: this table has one clock.
//
// Locks: the singleton row, after the feed lock and the consumer's cursor row
// (the feed pass is its only caller) and before any lane lock of the same
// pass; a sweep slice takes the row only in its own short transactions, never
// with a lane lock held (lockorder.go).
func bumpYTLinksSweepTx(ctx context.Context, tx pgx.Tx, now time.Time) error {
	var generation int64
	if err := tx.QueryRow(ctx, `INSERT INTO yt_links_sweep(singleton, generation, done_generation, after_task_id, updated_at)
		VALUES (TRUE, 1, 0, 0, $1)
		ON CONFLICT (singleton) DO UPDATE SET generation = yt_links_sweep.generation + 1, after_task_id = 0, updated_at = EXCLUDED.updated_at
		RETURNING generation`, now.UTC()).Scan(&generation); err != nil {
		return err
	}
	log.Printf("ytwrite.links_sweep_generation generation=%d", generation)
	return nil
}

// YTLinksSweepPass is what one RunYTLinksSweep did. Idle: the mode is off or
// no walk is owed. Restarted: the generation moved (or another worker moved
// the walk) while the slice ran, so the slice's position was discarded and the
// next pass walks again from the position the row now holds. Complete: the
// slice reached the end of the bound tasks at Generation. Visited counts the
// tasks the slice read, Prepared the ops it prepared.
type YTLinksSweepPass struct {
	Generation int64
	Visited    int
	Prepared   int
	Idle       bool
	Restarted  bool
	Complete   bool
}

// RunYTLinksSweep runs one slice of the sweep at now: up to limit bound tasks
// of the mode's tenant after the walk's position, ascending, each in its own
// transaction under its lane lock through the consumer's rule
// (prepareYTLinksTx; an unchanged snapshot makes no op). It then advances the
// position, or marks the generation done when the slice ran out of tasks,
// both in one UPDATE fenced on the generation and the position the slice
// started from. With the mode off, or no walk owed, it does nothing.
//
// Locks: one transaction per task, holding that task's lane lock and what
// prepareYTSnapshotTx takes after it; the position update is its own
// transaction holding only the sweep row. No transaction holds the row and a
// lane lock together.
func (s *Store) RunYTLinksSweep(ctx context.Context, now time.Time, limit int) (YTLinksSweepPass, error) {
	pass := YTLinksSweepPass{Idle: true}
	mode, tenant := s.ytLinksNow()
	if mode != YTLinksOn || limit < 1 {
		return pass, nil
	}
	st, err := ytLinksSweepStateTx(ctx, s.pool)
	if err != nil {
		return pass, err
	}
	if st.Complete() {
		return pass, nil
	}
	pass.Idle, pass.Generation = false, st.Generation
	rows, err := s.pool.Query(ctx, `SELECT task_id FROM yt_task_bindings WHERE tenant=$1 AND task_id > $2 ORDER BY task_id LIMIT $3`,
		tenant, st.AfterTaskID, limit)
	if err != nil {
		return pass, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return pass, err
	}
	for _, id := range ids {
		outcome, err := s.sweepYTLinksTask(ctx, now, tenant, id)
		if err != nil {
			return pass, err
		}
		pass.Visited++
		if s.ytLinksSweepVisited != nil {
			s.ytLinksSweepVisited(id)
		}
		if outcome == ytLinksPrepared {
			pass.Prepared++
		}
	}
	last := st.AfterTaskID
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	var tag pgconn.CommandTag
	if len(ids) < limit {
		pass.Complete = true
		tag, err = s.pool.Exec(ctx, `UPDATE yt_links_sweep SET done_generation = $1, after_task_id = 0, updated_at = $2
			WHERE singleton AND generation = $1 AND after_task_id = $3`, st.Generation, now.UTC(), st.AfterTaskID)
	} else {
		tag, err = s.pool.Exec(ctx, `UPDATE yt_links_sweep SET after_task_id = $4, updated_at = $2
			WHERE singleton AND generation = $1 AND after_task_id = $3`, st.Generation, now.UTC(), st.AfterTaskID, last)
	}
	if err != nil {
		return pass, err
	}
	if tag.RowsAffected() == 0 {
		pass.Restarted, pass.Complete = true, false
	}
	log.Printf("ytwrite.links_sweep generation=%d visited=%d prepared=%d complete=%t restarted=%t last_task_id=%d",
		pass.Generation, pass.Visited, pass.Prepared, pass.Complete, pass.Restarted, last)
	return pass, nil
}

// sweepYTLinksTask is prepareYTLinksTx in its own transaction.
func (s *Store) sweepYTLinksTask(ctx context.Context, now time.Time, tenant string, id int64) (ytLinksOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	outcome, err := prepareYTLinksTx(ctx, tx, now, tenant, id)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}

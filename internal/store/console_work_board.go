package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReadTaskWorkBoards overlays immutable placements and job completion onto the
// durable board projection. It works with Linear delivery disabled and never
// writes projection/outbox rows. Queries are batched for the whole UI page.
func (s *Store) ReadTaskWorkBoards(ctx context.Context, tasks []Task) (map[int64]LinearBoard, error) {
	boards := make(map[int64]LinearBoard, len(tasks))
	if len(tasks) == 0 {
		return boards, nil
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	// One read snapshot keeps placement and terminal-job observations consistent.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT task_id,status FROM linear_board_tasks WHERE task_id=ANY($1) ORDER BY task_id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var board LinearBoard
		if err := json.Unmarshal(raw, &board); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read work board %d: %w", id, err)
		}
		boards[id] = board
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		board := boards[task.ID]
		board.ApplyTask(task)
		boards[task.ID] = board
	}
	rows, err = tx.Query(ctx, `SELECT `+placementColumns+` FROM placement_decisions WHERE task_id=ANY($1) AND outcome='placed' ORDER BY decided_at,id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		placement, err := scanPlacement(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if placement.TaskID == nil || placement.ChosenHost == nil {
			rows.Close()
			return nil, fmt.Errorf("placed decision %d lacks owner or host", placement.ID)
		}
		board := boards[*placement.TaskID]
		board.ApplyPlacement(placement)
		boards[*placement.TaskID] = board
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	owners := map[string][]int64{}
	jobIDs := []string{}
	for id, board := range boards {
		for _, session := range board.Sessions {
			if _, ok := owners[session.JobID]; !ok {
				jobIDs = append(jobIDs, session.JobID)
			}
			owners[session.JobID] = append(owners[session.JobID], id)
		}
	}
	if len(jobIDs) > 0 {
		rows, err = tx.Query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=ANY($1) ORDER BY job_id`, jobIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			job, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			for _, id := range owners[job.JobID] {
				board := boards[id]
				board.ApplyJob(job)
				boards[id] = board
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return boards, tx.Commit(ctx)
}

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskWorkSession is one job that worked on a task: its placement (host,
// model, role) and whether it is still running.
type TaskWorkSession struct {
	JobID   string
	Host    string
	Family  string
	Model   string
	Role    string
	Active  bool
	EndedAt time.Time
	At      time.Time
}

// TaskWorkBoard is the console's per-task work overlay: the task's job, PR
// and head, and the sessions placed for it. It is derived on read from the
// task, placement_decisions and jobs; nothing stores it (hk 4425 retired the
// Linear board projection it used to start from).
type TaskWorkBoard struct {
	JobID    string
	PR       string
	HeadSHA  string
	Sessions []TaskWorkSession
}

// applyPlacement records a placed decision; a later placement of the same
// job replaces the earlier one, keeping an end already observed after it.
func (b *TaskWorkBoard) applyPlacement(d PlacementDecision) {
	session := TaskWorkSession{JobID: d.JobID, Host: *d.ChosenHost, Family: d.Family, Model: d.Model, Role: d.Role, Active: true, At: d.DecidedAt}
	for i, old := range b.Sessions {
		if old.JobID != d.JobID {
			continue
		}
		if old.At.After(d.DecidedAt) {
			return
		}
		if !old.Active && !old.EndedAt.Before(d.DecidedAt) {
			session.Active, session.EndedAt = false, old.EndedAt
		}
		b.Sessions[i] = session
		return
	}
	b.Sessions = append(b.Sessions, session)
}

// applyJob ends the job's sessions once the job is terminal.
func (b *TaskWorkBoard) applyJob(job Job) {
	if !jobTerminal[job.State] {
		return
	}
	for i := range b.Sessions {
		s := &b.Sessions[i]
		if s.JobID == job.JobID && !job.UpdatedAt.Before(s.At) {
			s.Active, s.EndedAt = false, job.UpdatedAt
		}
	}
}

// PrimaryJob is the task's job, else the latest lead or worker placed for
// it (a placement can arrive before the claim sets refs.job_id).
func (b TaskWorkBoard) PrimaryJob() string {
	if b.JobID != "" {
		return b.JobID
	}
	var latest TaskWorkSession
	for _, s := range b.Sessions {
		if (s.Role == "lead" || s.Role == "worker") && (latest.JobID == "" || s.At.After(latest.At)) {
			latest = s
		}
	}
	return latest.JobID
}

// ReadTaskWorkBoards derives the work overlay of every task in one read
// snapshot: placed decisions owned by the tasks, then the jobs they name.
// It writes nothing. Queries are batched for the whole UI page.
func (s *Store) ReadTaskWorkBoards(ctx context.Context, tasks []Task) (map[int64]TaskWorkBoard, error) {
	boards := make(map[int64]TaskWorkBoard, len(tasks))
	if len(tasks) == 0 {
		return boards, nil
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
		boards[task.ID] = TaskWorkBoard{JobID: task.Refs.JobID, PR: task.Refs.PR, HeadSHA: task.Refs.HeadSHA}
	}
	// One read snapshot keeps placement and terminal-job observations consistent.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+placementColumns+` FROM placement_decisions WHERE task_id=ANY($1) AND outcome='placed' ORDER BY decided_at,id`, ids)
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
		board.applyPlacement(placement)
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
				board.applyJob(job)
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

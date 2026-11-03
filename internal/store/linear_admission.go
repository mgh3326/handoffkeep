package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LinearCreateLease owns the cross-process create lock. Reservations commit
// before HTTP: an ambiguous remote acceptance survives a process crash.
type LinearCreateLease interface {
	Task(context.Context, int64) (Task, bool, error)
	Pending(context.Context) ([]string, error)
	Reserve(context.Context, string) error
	Reject(context.Context, string) error
	RejectionKnown(context.Context, string) (bool, error)
	CapacityHold(context.Context) (int, bool, error)
	HoldCapacity(context.Context, int) error
	ClearCapacityHold(context.Context) error
	Complete(context.Context, string) error
	ReserveBoard(context.Context, string, int64) (bool, error)
	AwaitBoardWebhook(context.Context, string) error
	LinearBoardReservation(context.Context, string) (LinearBoardReservationState, error)
	ReservedCreates(context.Context) (int, error)
	ReleaseAbsent(context.Context, string) error
}

type linearCreateLease struct{ conn *pgxpool.Conn }

func (l linearCreateLease) Task(ctx context.Context, id int64) (Task, bool, error) {
	var task Task
	err := scanTask(l.conn.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, id), &task)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	return task, err == nil, err
}

func (l linearCreateLease) Pending(ctx context.Context) ([]string, error) {
	rows, err := l.conn.Query(ctx, `SELECT issue_id FROM linear_create_admissions WHERE NOT awaiting_webhook ORDER BY issue_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (l linearCreateLease) Reserve(ctx context.Context, id string) error {
	_, err := l.conn.Exec(ctx, `INSERT INTO linear_create_admissions(issue_id,created_at) VALUES($1,now()) ON CONFLICT DO NOTHING`, id)
	return err
}
func (l linearCreateLease) Complete(ctx context.Context, id string) error {
	_, err := l.conn.Exec(ctx, `DELETE FROM linear_create_admissions WHERE issue_id=$1`, id)
	return err
}

// SerializeLinearCreate uses one dedicated connection with a session lock so
// a reservation can commit while the lock spans count + create. On an unlock
// failure the connection is closed, never returned with a lock to the pool.
func (s *Store) SerializeLinearCreate(ctx context.Context, run func(LinearCreateLease) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lockID int64 = 824180175
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		// Cancellation may race lock acquisition; discard the session.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(cleanup)
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(cleanup, `SELECT pg_advisory_unlock($1)`, lockID).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Hijack().Close(cleanup)
		}
	}()
	return run(linearCreateLease{conn: conn})
}

func (l linearCreateLease) Reject(ctx context.Context, id string) error {
	_, err := l.conn.Exec(ctx, `UPDATE linear_create_admissions SET uncertain=false WHERE issue_id=$1`, id)
	return err
}
func (l linearCreateLease) RejectionKnown(ctx context.Context, id string) (bool, error) {
	var rejected bool
	err := l.conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_create_admissions WHERE issue_id=$1 AND NOT uncertain)`, id).Scan(&rejected)
	return rejected, err
}

func (l linearCreateLease) CapacityHold(ctx context.Context) (int, bool, error) {
	var limit int
	var held bool
	err := l.conn.QueryRow(ctx, `SELECT policy_limit,held FROM linear_capacity_state WHERE singleton`).Scan(&limit, &held)
	return limit, held, err
}
func (l linearCreateLease) HoldCapacity(ctx context.Context, limit int) error {
	_, err := l.conn.Exec(ctx, `UPDATE linear_capacity_state SET held=true,policy_limit=$1 WHERE singleton`, limit)
	return err
}
func (l linearCreateLease) ClearCapacityHold(ctx context.Context) error {
	_, err := l.conn.Exec(ctx, `UPDATE linear_capacity_state SET held=false WHERE singleton`)
	return err
}

func (l linearCreateLease) ReserveBoard(ctx context.Context, id string, taskID int64) (bool, error) {
	if taskID < 1 || id != LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", taskID)) {
		return false, ErrLinearIssueOwnership
	}
	tag, err := l.conn.Exec(ctx, `INSERT INTO linear_create_admissions(issue_id,created_at,board_task_id) VALUES($1,now(),$2) ON CONFLICT DO NOTHING`, id, taskID)
	return tag.RowsAffected() == 1, err
}
func (l linearCreateLease) AwaitBoardWebhook(ctx context.Context, id string) error {
	_, err := l.conn.Exec(ctx, `UPDATE linear_create_admissions SET awaiting_webhook=true WHERE issue_id=$1`, id)
	return err
}

// Pending is an unresolved create outcome; AwaitingWebhook is an accepted
// create. Both book capacity, but only the former needs an operator signal.
type LinearBoardReservationState int

const (
	LinearBoardReservationNone LinearBoardReservationState = iota
	LinearBoardReservationPending
	LinearBoardReservationAwaitingWebhook
)

const linearBoardReservationQuery = `SELECT awaiting_webhook FROM linear_create_admissions WHERE issue_id=$1 AND board_task_id IS NOT NULL`

func scanLinearBoardReservation(row pgx.Row) (LinearBoardReservationState, error) {
	var awaiting bool
	if err := row.Scan(&awaiting); errors.Is(err, pgx.ErrNoRows) {
		return LinearBoardReservationNone, nil
	} else if err != nil {
		return LinearBoardReservationNone, err
	}
	if awaiting {
		return LinearBoardReservationAwaitingWebhook, nil
	}
	return LinearBoardReservationPending, nil
}

func (l linearCreateLease) LinearBoardReservation(ctx context.Context, id string) (LinearBoardReservationState, error) {
	return scanLinearBoardReservation(l.conn.QueryRow(ctx, linearBoardReservationQuery, id))
}

func (s *Store) LinearBoardReservation(ctx context.Context, id string) (LinearBoardReservationState, error) {
	return scanLinearBoardReservation(s.pool.QueryRow(ctx, linearBoardReservationQuery, id))
}

func (l linearCreateLease) ReservedCreates(ctx context.Context) (int, error) {
	var count int
	err := l.conn.QueryRow(ctx, `SELECT count(*) FROM linear_create_admissions`).Scan(&count)
	return count, err
}

// A confirmed exact-id absence releases an old ambiguous reservation. The
// grace period exceeds the HTTP/create deadline; the create lock excludes an
// in-flight local writer. Every board reservation gets this grace, including
// legacy known-rejection rows; only non-board known rejections bypass it.
var ErrLinearReservationTooRecent = errors.New("linear_reservation_too_recent")

func (l linearCreateLease) ReleaseAbsent(ctx context.Context, id string) error {
	var recent bool
	if err := l.conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_create_admissions WHERE issue_id=$1 AND (board_task_id IS NOT NULL OR uncertain) AND created_at>clock_timestamp()-interval '2 minutes')`, id).Scan(&recent); err != nil {
		return err
	}
	if recent {
		return ErrLinearReservationTooRecent
	}
	return l.Complete(ctx, id)
}

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerLock identifies a session lock held by one serve worker.
type WorkerLock int64

const (
	HubForwardWorkerLock  WorkerLock = 8241873510
	LinearBoardWorkerLock WorkerLock = 8241873511
)

// TryWorkerLock reserves a connection for the lifetime of a worker. A failed
// unlock discards the connection so a locked session cannot return to the pool.
func (s *Store) TryWorkerLock(ctx context.Context, id WorkerLock) (func(), bool, error) {
	if id != HubForwardWorkerLock && id != LinearBoardWorkerLock {
		return nil, false, errors.New("unknown worker lock")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var held bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(id)).Scan(&held); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !held {
		conn.Release()
		return nil, false, nil
	}
	return func() { releaseWorkerLock(conn, id) }, true, nil
}

func releaseWorkerLock(conn *pgxpool.Conn, id WorkerLock) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, int64(id)).Scan(&unlocked)
	if err != nil || !unlocked {
		_ = conn.Hijack().Close(ctx)
		return
	}
	conn.Release()
}

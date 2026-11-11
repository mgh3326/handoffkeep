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
	HubForwardWorkerLock    WorkerLock = 8241873510
	LinearBoardWorkerLock   WorkerLock = 8241873511
	TaskThreadWorkerLock    WorkerLock = 8241873512
	workerLockProbeInterval            = time.Second
	workerLockProbeTimeout             = 2 * time.Second
)

// WorkerLease holds the connection for a session lock.
type WorkerLease struct{ conn *pgxpool.Conn }

// TryWorkerLock reserves a connection for the lifetime of a worker. An
// uncertain acquisition discards the connection so a locked session cannot
// return to the pool.
func (s *Store) TryWorkerLock(ctx context.Context, id WorkerLock) (*WorkerLease, bool, error) {
	if id != HubForwardWorkerLock && id != LinearBoardWorkerLock && id != TaskThreadWorkerLock {
		return nil, false, errors.New("unknown worker lock")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var held bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(id)).Scan(&held); err != nil {
		discardWorkerConnection(conn)
		return nil, false, err
	}
	if !held {
		conn.Release()
		return nil, false, nil
	}
	return &WorkerLease{conn: conn}, true, nil
}

// Watch returns if the session fails or ctx is canceled. The worker must stop
// before Release returns the connection to the pool.
func (l *WorkerLease) Watch(ctx context.Context) error {
	ticker := time.NewTicker(workerLockProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			probe, cancel := context.WithTimeout(ctx, workerLockProbeTimeout)
			var one int
			err := l.conn.QueryRow(probe, `SELECT 1`).Scan(&one)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// Release closes the dedicated connection, dropping its session lock.
func (l *WorkerLease) Release() { discardWorkerConnection(l.conn) }

func discardWorkerConnection(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), workerLockProbeTimeout)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}

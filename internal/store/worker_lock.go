package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerLock identifies a session lock held by one serve worker.
type WorkerLock int64

const (
	HubForwardWorkerLock WorkerLock = 8241873510
	// 8241873511 was the retired Linear board worker's lock (hk 4425); do
	// not reuse it while a previous release may still hold it.
	TaskThreadWorkerLock WorkerLock = 8241873512
	// DriftWorkerLock is the daily drift and hygiene worker's lock (hk 5112
	// track B-3): it ticks every 5 minutes and delivers once on the first
	// tick at or after 09:00 KST.
	DriftWorkerLock WorkerLock = 8241873520
	// LaneCheckWorkerLock is the per-lane wake check's lock (operator
	// decision 418, hk 5201): it ticks every 15 minutes and queues one
	// lane_check notice per lane whose findings need a look.
	LaneCheckWorkerLock WorkerLock = 8241873521
	// TableStatsWorkerLock is the daily MySQL table snapshot's lock (hk 6231):
	// it ticks every 5 minutes and takes at most one snapshot a day.
	TableStatsWorkerLock    WorkerLock = 8241873522
	workerLockProbeInterval            = time.Second
	workerLockProbeTimeout             = 2 * time.Second
)

// sessionWorkerLocks is every lock a serve worker may hold for its lifetime.
// Each one keeps a pooled connection checked out, so the pool serve opens must
// be at least this many connections plus query headroom.
var sessionWorkerLocks = []WorkerLock{
	HubForwardWorkerLock, TaskThreadWorkerLock, DriftWorkerLock, LaneCheckWorkerLock, TableStatsWorkerLock,
}

// workerPoolHeadroom is what is left for store queries and API requests after
// every session lock is held.
const workerPoolHeadroom = 4

// workerPoolFloor is the smallest pool serve may open: one connection per
// session-lock worker plus headroom.
var workerPoolFloor = int32(len(sessionWorkerLocks) + workerPoolHeadroom)

func knownWorkerLock(id WorkerLock) bool {
	for _, lock := range sessionWorkerLocks {
		if id == lock {
			return true
		}
	}
	return false
}

// WorkerLease holds the connection for a session lock.
type WorkerLease struct {
	conn      *pgxpool.Conn
	mysqlConn *sql.Conn
	mysqlName string
	lockName  string
}

// LockName is a diagnostic identity for the acquired session lock.
func (l *WorkerLease) LockName() string {
	if l == nil {
		return ""
	}
	return l.lockName
}

// TryWorkerLock reserves a connection for the lifetime of a worker. An
// uncertain acquisition discards the connection so a locked session cannot
// return to the pool.
func (s *Store) TryWorkerLock(ctx context.Context, id WorkerLock) (*WorkerLease, bool, error) {
	if !knownWorkerLock(id) {
		return nil, false, errors.New("unknown worker lock")
	}
	if s.dialect == mysqlDialect {
		conn, name, held, err := mysqlTryWorkerSessionLock(ctx, s.mysqlPool, id)
		if err != nil || !held {
			return nil, held, err
		}
		return &WorkerLease{mysqlConn: conn, mysqlName: name, lockName: name}, true, nil
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
	return &WorkerLease{conn: conn, lockName: "pg_advisory:" + strconv.FormatInt(int64(id), 10)}, true, nil
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
			var err error
			if l.mysqlConn != nil {
				err = l.mysqlConn.QueryRowContext(probe, `SELECT 1`).Scan(&one)
			} else {
				err = l.conn.QueryRow(probe, `SELECT 1`).Scan(&one)
			}
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// Release closes the dedicated connection, dropping its session lock. A
// second call is a no-op: the first call already returned the connection to
// the pool, and the lease no longer names a live connection for either
// dialect.
func (l *WorkerLease) Release() {
	if l == nil {
		return
	}
	if l.mysqlConn != nil {
		_ = mysqlReleaseSessionLock(l.mysqlConn, l.mysqlName)
		l.mysqlConn = nil
		l.mysqlName = ""
		return
	}
	if l.conn == nil {
		return
	}
	discardWorkerConnection(l.conn)
	l.conn = nil
}

func discardWorkerConnection(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), workerLockProbeTimeout)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}

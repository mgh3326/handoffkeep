package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"github.com/jackc/pgx/v5"
	"strconv"
	"time"
)

// begin starts a write transaction on either dialect. Always pair it with
//
//	tx, release, err := s.begin(ctx)
//	if err != nil { ... }
//	defer release()
//	defer tx.Rollback(ctx)
//
// so the rollback runs before the release.
//
// PostgreSQL is pool.Begin: its deferred sync allocator runs inside COMMIT.
// On MySQL the transaction pins one connection, sets @hk_txn so the emit
// triggers can tag their sync_log rows, and Commit numbers those rows
// (mysqlAllocateSync) immediately before it commits. Every write to tasks,
// documents, task_comments or decisions on MySQL must go through begin or
// mysqlWriteTransaction, never through s.db().Exec.
func (s *Store) begin(ctx context.Context) (transaction, func(), error) {
	if s.dialect != mysqlDialect {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, nil, mapDriverError(err)
		}
		return pgTransaction{tx}, func() {}, nil
	}
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var token [8]byte
	if _, err = rand.Read(token[:]); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, mapDriverError(err)
	}
	// Cleanup runs once, after the caller's rollback: clear the identity so a
	// pooled connection never tags another transaction's rows, and discard the
	// physical connection when that is uncertain.
	release := func() {
		_ = tx.Rollback()
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, `SET @hk_txn = NULL`); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}
	if _, err = tx.ExecContext(ctx, `SET @hk_txn = ?`, token[:]); err != nil {
		release()
		return nil, nil, mapDriverError(err)
	}
	return &mysqlWriteTx{sqlTransaction: sqlTransaction{tx}, raw: tx, token: token[:]}, release, nil
}

// mysqlWriteTx numbers the transaction's pending sync_log rows at commit.
type mysqlWriteTx struct {
	sqlTransaction
	raw   *sql.Tx
	token []byte
}

func (t *mysqlWriteTx) Commit(ctx context.Context) error {
	if err := mysqlAllocateSync(ctx, t.raw, t.token); err != nil {
		return mapDriverError(err)
	}
	return mapDriverError(t.raw.Commit())
}

// retryTx runs one whole transaction function, and on MySQL runs it once more
// after a deadlock or lock-wait timeout (design §11.1). f must open its own
// transaction with begin. PostgreSQL never retries here: its callers already
// keep the single-attempt behaviour they had.
func (s *Store) retryTx(ctx context.Context, f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		if !s.retryTransaction(ctx, attempt, err) {
			return err
		}
	}
}

// lockTx takes the transaction-scoped lock for a numeric lock id: the
// advisory lock on PostgreSQL, the hk_locks row on MySQL. Both are held until
// commit or rollback and the ids and order are those of lockorder.go.
func (s *Store) lockTx(ctx context.Context, tx transaction, id int64) error {
	if s.dialect == mysqlDialect {
		_, err := mysqlTransactionLock(ctx, tx, mysqlLockKey("id", strconv.FormatInt(id, 10)), false)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, id)
	return err
}

// lockWorkerOperationTx serializes one worker's check-and-write operation in
// the current database. Session worker locks gate a whole worker generation;
// this transaction lock also protects a brief overlap between generations.
func (s *Store) lockWorkerOperationTx(ctx context.Context, tx transaction, operation string) error {
	key := "worker:" + operation
	if s.dialect == mysqlDialect {
		_, err := mysqlTransactionLock(ctx, tx, mysqlLockKey("worker", operation), false)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// collectRows is pgx.CollectRows over the store's driver-neutral rows: it
// closes rows, and an empty result is a non-nil empty slice, so JSON stays [].
func collectRows[T any](r rows, f func(row) (T, error)) ([]T, error) {
	defer r.Close()
	out := []T{}
	for r.Next() {
		v, err := f(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rowTo is pgx.RowTo for one scalar column.
func rowTo[T any](r row) (T, error) {
	var v T
	err := r.Scan(&v)
	return v, err
}

// pgxTx unwraps a PostgreSQL transaction for a helper whose area is not ported
// yet and still takes pgx.Tx. Every caller first refuses a MySQL store with
// ErrDialectUnsupported, so reaching the panic means a missing guard.
func pgxTx(tx transaction) pgx.Tx {
	if p, ok := tx.(pgTransaction); ok {
		return p.tx
	}
	panic("store: PostgreSQL-only helper reached on another dialect")
}

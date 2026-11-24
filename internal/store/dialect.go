package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Driver-neutral results keep the store's SQL and transaction logic independent
// of either driver's concrete types.
type row interface{ Scan(...any) error }
type rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}
type querier interface {
	Exec(context.Context, string, ...any) (int64, error)
	Query(context.Context, string, ...any) (rows, error)
	QueryRow(context.Context, string, ...any) row
	Begin(context.Context) (transaction, error)
}
type transaction interface {
	InsertID(context.Context, string, ...any) (int64, error)
	Exec(context.Context, string, ...any) (int64, error)
	Query(context.Context, string, ...any) (rows, error)
	QueryRow(context.Context, string, ...any) row
	// Begin opens a savepoint inside the transaction: Commit releases it and
	// Rollback undoes only the work since it, leaving the outer transaction open.
	Begin(context.Context) (transaction, error)
	Commit(context.Context) error
	Rollback(context.Context) error
}

var (
	ErrNoRows               = pgx.ErrNoRows
	errUniqueViolation      = errors.New("store: unique violation")
	errForeignKeyViolation  = errors.New("store: foreign key violation")
	errCheckViolation       = errors.New("store: check violation")
	errSerializationFailure = errors.New("store: serialization or deadlock failure")
	errDeadlock             = fmt.Errorf("%w: deadlock", errSerializationFailure)
)

// mapDriverError preserves the underlying error for diagnostics and existing
// driver-specific callers while making common failures matchable across drivers.
func mapDriverError(err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		sentinel = ErrNoRows
	}
	var pe *pgconn.PgError
	var me *mysql.MySQLError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			sentinel = errUniqueViolation
		case "23503":
			sentinel = errForeignKeyViolation
		case "23514":
			sentinel = errCheckViolation
		case "40001":
			sentinel = errSerializationFailure
		case "40P01":
			sentinel = errDeadlock
		}
		// This PostgreSQL path distinguishes one named unique constraint.
		// The decisions area must supply the MySQL equivalent when ported.
		if pe.ConstraintName == "decision_answer_attempts_one_deny" {
			sentinel = errors.Join(sentinel, ErrDecisionAttemptDenied)
		}
	} else if errors.As(err, &me) {
		switch me.Number {
		case 1062:
			sentinel = errUniqueViolation
		case 1451, 1452:
			sentinel = errForeignKeyViolation
		case 3819, 4025:
			sentinel = errCheckViolation
		case 1205:
			sentinel = errSerializationFailure
		case 1213:
			sentinel = errDeadlock
		}
	}
	if sentinel == nil {
		return err
	}
	if errors.Is(err, sentinel) {
		return err
	}
	return mappedError{err: err, sentinel: sentinel}
}

// documentBodyWriteError maps PostgreSQL's refusal of a document body that has
// too many distinct words for the documents full-text index (SQLSTATE 54000,
// "string is too long for tsvector") to ErrDocumentTooLarge. The match takes
// both the code and the message; any other error passes through unchanged. The
// reason is store-authored: the driver's text is not echoed.
func documentBodyWriteError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "54000" && strings.Contains(pe.Message, "too long for tsvector") {
		return fmt.Errorf("%w: body has too many distinct words to index", ErrDocumentTooLarge)
	}
	return err
}

// mappedError retains the driver's diagnostic text and unwrap chain.
type mappedError struct{ err, sentinel error }

func (e mappedError) Error() string        { return e.err.Error() }
func (e mappedError) Unwrap() error        { return e.err }
func (e mappedError) Is(target error) bool { return errors.Is(e.sentinel, target) }

type mappedRow struct{ row row }

func (r mappedRow) Scan(dest ...any) error { return mapDriverError(r.row.Scan(dest...)) }

type mappedRows struct{ rows rows }

func (r mappedRows) Next() bool             { return r.rows.Next() }
func (r mappedRows) Scan(dest ...any) error { return mapDriverError(r.rows.Scan(dest...)) }
func (r mappedRows) Err() error             { return mapDriverError(r.rows.Err()) }
func (r mappedRows) Close()                 { r.rows.Close() }

type pgQuerier struct{ pool *pgxpool.Pool }

func (p pgQuerier) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	t, e := p.pool.Exec(ctx, q, args...)
	return t.RowsAffected(), mapDriverError(e)
}
func (p pgQuerier) Query(ctx context.Context, q string, args ...any) (rows, error) {
	r, e := p.pool.Query(ctx, q, args...)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return mappedRows{r}, nil
}
func (p pgQuerier) QueryRow(ctx context.Context, q string, args ...any) row {
	return mappedRow{p.pool.QueryRow(ctx, q, args...)}
}
func (p pgQuerier) Begin(ctx context.Context) (transaction, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return pgTransaction{tx}, nil
}

type pgTransaction struct{ tx pgx.Tx }

func (p pgTransaction) InsertID(ctx context.Context, q string, args ...any) (int64, error) {
	var id int64
	err := p.tx.QueryRow(ctx, q, args...).Scan(&id)
	return id, mapDriverError(err)
}
func (p pgTransaction) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	t, e := p.tx.Exec(ctx, q, args...)
	return t.RowsAffected(), mapDriverError(e)
}
func (p pgTransaction) Query(ctx context.Context, q string, args ...any) (rows, error) {
	r, e := p.tx.Query(ctx, q, args...)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return mappedRows{r}, nil
}
func (p pgTransaction) QueryRow(ctx context.Context, q string, args ...any) row {
	return mappedRow{p.tx.QueryRow(ctx, q, args...)}
}
func (p pgTransaction) Begin(ctx context.Context) (transaction, error) {
	sp, err := p.tx.Begin(ctx)
	if err != nil {
		return nil, mapDriverError(err)
	}
	return pgTransaction{sp}, nil
}
func (p pgTransaction) Commit(ctx context.Context) error   { return mapDriverError(p.tx.Commit(ctx)) }
func (p pgTransaction) Rollback(ctx context.Context) error { return mapDriverError(p.tx.Rollback(ctx)) }

type sqlRows struct{ *sql.Rows }

func (r sqlRows) Close() { _ = r.Rows.Close() }

type sqlQuerier struct{ db *sql.DB }

func (s sqlQuerier) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, e := s.db.ExecContext(ctx, q, args...)
	return sqlResult(r, e)
}
func (s sqlQuerier) Query(ctx context.Context, q string, args ...any) (rows, error) {
	r, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return mappedRows{sqlRows{r}}, nil
}
func (s sqlQuerier) QueryRow(ctx context.Context, q string, args ...any) row {
	return mappedRow{s.db.QueryRowContext(ctx, q, args...)}
}
func (s sqlQuerier) Begin(ctx context.Context) (transaction, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return sqlTransaction{tx}, nil
}

type sqlTransaction struct{ tx *sql.Tx }

func (s sqlTransaction) InsertID(ctx context.Context, q string, args ...any) (int64, error) {
	result, err := s.tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, mapDriverError(err)
	}
	id, err := result.LastInsertId()
	return id, mapDriverError(err)
}
func (s sqlTransaction) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, e := s.tx.ExecContext(ctx, q, args...)
	return sqlResult(r, e)
}
func (s sqlTransaction) Query(ctx context.Context, q string, args ...any) (rows, error) {
	r, e := s.tx.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return mappedRows{sqlRows{r}}, nil
}
func (s sqlTransaction) QueryRow(ctx context.Context, q string, args ...any) row {
	return mappedRow{s.tx.QueryRowContext(ctx, q, args...)}
}

// savepointSeq names MySQL savepoints; a name only has to be unique within the
// connection's transaction, and a process-wide counter is.
var savepointSeq atomic.Int64

// Begin opens a savepoint on the same transaction connection.
func (s sqlTransaction) Begin(ctx context.Context) (transaction, error) {
	name := fmt.Sprintf("hk_sp_%d", savepointSeq.Add(1))
	if _, err := s.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return nil, mapDriverError(err)
	}
	return &sqlSavepoint{sqlTransaction: s, name: name}, nil
}

// sqlSavepoint keeps the outer transaction's methods for statements; only its
// Commit and Rollback differ.
type sqlSavepoint struct {
	sqlTransaction
	name string
	done bool
}

func (p *sqlSavepoint) Commit(ctx context.Context) error {
	p.done = true
	_, err := p.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+p.name)
	return mapDriverError(err)
}

// Rollback after Commit is a no-op, as for a database transaction.
func (p *sqlSavepoint) Rollback(ctx context.Context) error {
	if p.done {
		return nil
	}
	p.done = true
	if _, err := p.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+p.name); err != nil {
		return mapDriverError(err)
	}
	_, err := p.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+p.name)
	return mapDriverError(err)
}

// sqlSnapshot is a MySQL read-only snapshot transaction (design §11.1): its
// connection already runs START TRANSACTION WITH CONSISTENT SNAPSHOT, READ
// ONLY at REPEATABLE READ, so every statement reads one fixed snapshot. It
// writes nothing, so like the snapshot helper it never takes @hk_txn. Commit
// and Rollback both release the pinned connection; Rollback after Commit is a
// no-op, as for a database transaction.
type sqlSnapshot struct {
	conn *sql.Conn
	done bool
}

func (s *sqlSnapshot) dialectOf() storeDialect { return mysqlDialect }

func (s *sqlSnapshot) InsertID(ctx context.Context, q string, args ...any) (int64, error) {
	result, err := s.conn.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, mapDriverError(err)
	}
	id, err := result.LastInsertId()
	return id, mapDriverError(err)
}

func (s *sqlSnapshot) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, e := s.conn.ExecContext(ctx, q, args...)
	return sqlResult(r, e)
}

func (s *sqlSnapshot) Query(ctx context.Context, q string, args ...any) (rows, error) {
	r, e := s.conn.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, mapDriverError(e)
	}
	return mappedRows{sqlRows{r}}, nil
}

func (s *sqlSnapshot) QueryRow(ctx context.Context, q string, args ...any) row {
	return mappedRow{s.conn.QueryRowContext(ctx, q, args...)}
}

// Begin opens a savepoint on the snapshot's connection.
func (s *sqlSnapshot) Begin(ctx context.Context) (transaction, error) {
	name := fmt.Sprintf("hk_sp_%d", savepointSeq.Add(1))
	if _, err := s.conn.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return nil, mapDriverError(err)
	}
	return &sqlSnapshotSavepoint{sqlSnapshot: s, name: name}, nil
}

func (s *sqlSnapshot) Commit(ctx context.Context) error {
	if s.done {
		return mapDriverError(sql.ErrTxDone)
	}
	s.done = true
	// A cancelled caller still needs its snapshot closed: without a live
	// context no COMMIT is sent and Close returns the connection to the pool
	// inside its read-only transaction, where later pool statements would
	// join it. Detach the cleanup from cancellation and bound it; a failed
	// COMMIT discards the connection instead of returning it to the pool.
	settle, cancel := snapshotCleanupContext(ctx)
	defer cancel()
	_, err := s.conn.ExecContext(settle, `COMMIT`)
	s.closeSnapshotConn(err == nil)
	return mapDriverError(err)
}

// snapshotCleanupTimeout bounds the detached COMMIT/ROLLBACK: once the caller
// is cancelled nothing else caps the cleanup (the MySQL DSN sets no read or
// write timeout), and an unanswered server would otherwise hold the request
// goroutine and the pinned connection indefinitely. A timeout here is safe:
// closeSnapshotConn discards the connection on failure.
const snapshotCleanupTimeout = 5 * time.Second

// snapshotCleanupContext detaches the snapshot cleanup from cancellation and
// bounds it, the same shape as the settle paths elsewhere in the store.
func snapshotCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
}

func (s *sqlSnapshot) Rollback(ctx context.Context) error {
	if s.done {
		return nil
	}
	s.done = true
	settle, cancel := snapshotCleanupContext(ctx)
	defer cancel()
	_, err := s.conn.ExecContext(settle, `ROLLBACK`)
	s.closeSnapshotConn(err == nil)
	return mapDriverError(err)
}

// closeSnapshotConn releases the pinned connection. A failed COMMIT or
// ROLLBACK discards it instead of returning it to the pool, so a connection
// still inside its snapshot never circulates.
func (s *sqlSnapshot) closeSnapshotConn(ok bool) {
	if !ok {
		_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = s.conn.Close()
}

// sqlSnapshotSavepoint keeps the snapshot's methods for statements; only its
// Commit and Rollback differ.
type sqlSnapshotSavepoint struct {
	*sqlSnapshot
	name string
	done bool
}

func (p *sqlSnapshotSavepoint) Commit(ctx context.Context) error {
	p.done = true
	_, err := p.conn.ExecContext(ctx, "RELEASE SAVEPOINT "+p.name)
	return mapDriverError(err)
}

// Rollback after Commit is a no-op, as for a database transaction.
func (p *sqlSnapshotSavepoint) Rollback(ctx context.Context) error {
	if p.done {
		return nil
	}
	p.done = true
	if _, err := p.conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+p.name); err != nil {
		return mapDriverError(err)
	}
	_, err := p.conn.ExecContext(ctx, "RELEASE SAVEPOINT "+p.name)
	return mapDriverError(err)
}

func (s sqlTransaction) Commit(context.Context) error   { return mapDriverError(s.tx.Commit()) }
func (s sqlTransaction) Rollback(context.Context) error { return mapDriverError(s.tx.Rollback()) }
func sqlResult(r sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, mapDriverError(err)
	}
	n, e := r.RowsAffected()
	return n, mapDriverError(e)
}

// placeholders builds an IN-list; an empty slice uses a false subquery instead
// of invalid IN () syntax, and has the same behavior as = ANY(empty array).
func placeholders(n int) string {
	if n == 0 {
		return "SELECT NULL WHERE FALSE"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// The zero dialect is PostgreSQL, including stores constructed directly by tests.
type storeDialect uint8

const (
	postgresDialect storeDialect = iota
	mysqlDialect
)

func (s *Store) q(postgres, mysql string) string {
	if s.dialect == mysqlDialect {
		return mysql
	}
	return postgres
}

func (s *Store) db() querier {
	if s.queries != nil {
		return s.queries
	}
	return pgQuerier{pool: s.pool}
}

// beginNamedTransaction pins a MySQL connection for a session lock and its
// transaction. Its cleanup runs after rollback or commit.
func (s *Store) beginNamedTransaction(ctx context.Context, key string) (transaction, func(), error) {
	db := s.db().(sqlQuerier).db
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256([]byte(key))
	name := fmt.Sprintf("hk:lock:%x", digest[:24])
	var acquired sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, name).Scan(&acquired)
	if err != nil || !acquired.Valid || acquired.Int64 != 1 {
		if err != nil || !acquired.Valid {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
		if err == nil {
			err = errors.New("acquire named transaction lock")
		}
		return nil, nil, err
	}
	release := func() {
		var released sql.NullInt64
		err := conn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, name).Scan(&released)
		if err != nil || !released.Valid || released.Int64 != 1 {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		release()
		return nil, nil, err
	}
	return sqlTransaction{tx}, release, nil
}

// Retry only a complete MySQL transaction after a deadlock or lock timeout.
// Its deferred rollback and session cleanup finish before the next attempt.
func (s *Store) retryTransaction(ctx context.Context, attempt int, err error) bool {
	// A nil store cannot be MySQL: validation tests call write methods on a
	// nil store and expect their input refusal before any store touch.
	return s != nil && s.dialect == mysqlDialect && attempt == 0 && ctx.Err() == nil && errors.Is(mapDriverError(err), errSerializationFailure)
}

// dialected is implemented by every querier and transaction adapter, so a
// helper that only receives its transaction (insertEvent, writeTaskTx, ...)
// can still choose its SQL text: sqlFor(tx, pgText, mysqlText).
type dialected interface{ dialectOf() storeDialect }

// *Store answers its own dialect, so sqlFor(s, …) picks the MySQL text on a
// MySQL store. Without it sqlFor fell back to PostgreSQL for every call that
// passes the store itself (main red at 66d8fa2, hk 5076).
func (s *Store) dialectOf() storeDialect      { return s.dialect }
func (pgQuerier) dialectOf() storeDialect     { return postgresDialect }
func (pgTransaction) dialectOf() storeDialect { return postgresDialect }
func (sqlQuerier) dialectOf() storeDialect    { return mysqlDialect }
func (sqlTransaction) dialectOf() storeDialect {
	return mysqlDialect
}

// dialectFor reports the dialect of a querier or transaction. Anything that is
// not one of the store's adapters is PostgreSQL, like the zero Store.
func dialectFor(x any) storeDialect {
	if d, ok := x.(dialected); ok {
		return d.dialectOf()
	}
	return postgresDialect
}

// sqlFor picks the statement text for the dialect of the querier or transaction
// the statement will run on.
func sqlFor(x any, postgres, mysql string) string {
	if dialectFor(x) == mysqlDialect {
		return mysql
	}
	return postgres
}

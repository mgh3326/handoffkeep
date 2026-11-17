package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

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
		if err != nil {
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
	return s.dialect == mysqlDialect && attempt == 0 && ctx.Err() == nil && errors.Is(mapDriverError(err), errSerializationFailure)
}

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapDriverError(t *testing.T) {
	for _, tc := range []struct{ err, want error }{
		{pgx.ErrNoRows, ErrNoRows}, {sql.ErrNoRows, ErrNoRows},
		{&pgconn.PgError{Code: "23505"}, errUniqueViolation}, {&mysql.MySQLError{Number: 1062}, errUniqueViolation},
		{&pgconn.PgError{Code: "23503"}, errForeignKeyViolation}, {&mysql.MySQLError{Number: 1452}, errForeignKeyViolation},
		{&pgconn.PgError{Code: "23514"}, errCheckViolation}, {&mysql.MySQLError{Number: 3819}, errCheckViolation},
		{&pgconn.PgError{Code: "40P01"}, errSerializationFailure}, {&mysql.MySQLError{Number: 1205}, errSerializationFailure},
	} {
		original := fmt.Errorf("query: %w", tc.err)
		got := mapDriverError(original)
		if got.Error() != original.Error() {
			t.Errorf("diagnostic changed: got %q want %q", got.Error(), original.Error())
		}
		if !errors.Is(got, tc.want) || !errors.Is(got, tc.err) {
			t.Errorf("mapDriverError(%v) = %v, want both %v and original", tc.err, got, tc.want)
		}
	}
	other := errors.New("other")
	if got := mapDriverError(other); got != other {
		t.Errorf("unknown error changed: %v", got)
	}
}

func TestPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "SELECT NULL WHERE FALSE"}, {1, "?"}, {3, "?,?,?"}} {
		if got := placeholders(tc.n); got != tc.want {
			t.Errorf("placeholders(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestRetryTransactionOnlyOnceOnMySQLConflict(t *testing.T) {
	mysqlStore := &Store{dialect: mysqlDialect}
	postgresStore := &Store{}
	for _, err := range []error{&mysql.MySQLError{Number: 1205}, &mysql.MySQLError{Number: 1213}} {
		if !mysqlStore.retryTransaction(context.Background(), 0, err) {
			t.Errorf("first MySQL conflict was not retried: %v", err)
		}
		if mysqlStore.retryTransaction(context.Background(), 1, err) {
			t.Errorf("second MySQL conflict was retried: %v", err)
		}
		if postgresStore.retryTransaction(context.Background(), 0, err) {
			t.Errorf("PostgreSQL path was retried: %v", err)
		}
	}
	if mysqlStore.retryTransaction(context.Background(), 0, errors.New("ordinary error")) {
		t.Fatal("ordinary error was retried")
	}
}

// TestRetryTxRetriesDeadlockOnce injects one 1213 through retryTx and sees
// exactly one retry: two attempts then success. A conflict that persists is
// an error after the second attempt, never a third one (design §11.1).
func TestRetryTxRetriesDeadlockOnce(t *testing.T) {
	mysqlStore := &Store{dialect: mysqlDialect}
	for _, number := range []uint16{1213, 1205} {
		conflict := &mysql.MySQLError{Number: number, Message: "injected conflict"}
		calls := 0
		err := mysqlStore.retryTx(context.Background(), func() error {
			calls++
			if calls == 1 {
				return conflict
			}
			return nil
		})
		if err != nil {
			t.Fatalf("MySQL %d: retryTx returned %v, want nil after one retry", number, err)
		}
		if calls != 2 {
			t.Fatalf("MySQL %d: one injected conflict ran %d attempts, want exactly 2", number, calls)
		}
		calls = 0
		err = mysqlStore.retryTx(context.Background(), func() error {
			calls++
			return conflict
		})
		if err == nil {
			t.Fatalf("MySQL %d: persistent conflict returned nil, want the conflict", number)
		}
		if calls != 2 {
			t.Fatalf("MySQL %d: persistent conflict ran %d attempts, want exactly 2", number, calls)
		}
	}
	// PostgreSQL keeps its single-attempt behaviour: no retry even on 1213.
	postgresStore := &Store{}
	calls := 0
	err := postgresStore.retryTx(context.Background(), func() error {
		calls++
		return &mysql.MySQLError{Number: 1213, Message: "injected conflict"}
	})
	if err == nil {
		t.Fatal("PostgreSQL path returned nil for an injected 1213")
	}
	if calls != 1 {
		t.Fatalf("PostgreSQL path ran %d attempts, want exactly 1", calls)
	}
}

func TestPatchBenchRepOutcomeKeepsMySQLPortedPath(t *testing.T) {
	mysqlStore := &Store{dialect: mysqlDialect}
	_, err := mysqlStore.PatchBenchRepOutcome(context.Background(), 0, BenchRepOutcomePatch{})
	if err == nil || err.Error() != "invalid bench rep id" {
		t.Fatalf("PatchBenchRepOutcome error = %v, want MySQL validation error", err)
	}
}

func TestBeginNamedTransactionDiscardsConnectionOnNullLockResult(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      driver.Value
		wantClosed bool
	}{
		{name: "NULL", value: nil, wantClosed: true},
		{name: "timeout", value: int64(0), wantClosed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := make(chan struct{}, 1)
			db := sql.OpenDB(lockResultConnector{value: tc.value, closed: closed})
			defer db.Close()
			store := &Store{mysqlPool: db, dialect: mysqlDialect, queries: sqlQuerier{db}}

			tx, release, err := store.beginNamedTransaction(context.Background(), "test-lock")
			if err == nil || tx != nil || release != nil {
				t.Fatalf("beginNamedTransaction() = (tx nil: %t, release nil: %t, err: %v), want lock refusal", tx == nil, release == nil, err)
			}
			if tc.wantClosed {
				select {
				case <-closed:
				default:
					t.Fatal("connection with ambiguous lock result was returned to the pool")
				}
			} else if got := db.Stats().Idle; got != 1 {
				t.Fatalf("idle connections after unambiguous timeout = %d, want 1", got)
			}
		})
	}
}

type lockResultConnector struct {
	value  driver.Value
	closed chan struct{}
}

func (c lockResultConnector) Connect(context.Context) (driver.Conn, error) {
	return lockResultConn{value: c.value, closed: c.closed}, nil
}
func (c lockResultConnector) Driver() driver.Driver { return lockResultDriver{} }

type lockResultDriver struct{}

func (lockResultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected Driver.Open")
}

type lockResultConn struct {
	value  driver.Value
	closed chan struct{}
}

func (c lockResultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c lockResultConn) Close() error {
	c.closed <- struct{}{}
	return nil
}
func (c lockResultConn) Begin() (driver.Tx, error) { return nil, errors.New("unsupported") }
func (c lockResultConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &lockResultRows{value: c.value}, nil
}

type lockResultRows struct {
	value driver.Value
	read  bool
}

func (*lockResultRows) Columns() []string { return []string{"GET_LOCK"} }
func (*lockResultRows) Close() error      { return nil }
func (r *lockResultRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

func TestMapDecisionConstraintPreservesSpecificRefusal(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", ConstraintName: "decision_answer_attempts_one_deny"}
	mapped := mapDriverError(err)
	if !errors.Is(mapped, ErrDecisionAttemptDenied) || !errors.Is(mapped, errUniqueViolation) {
		t.Fatalf("constraint mapping lost its meaning: %v", mapped)
	}
	other := mapDriverError(&pgconn.PgError{Code: "23505", ConstraintName: "another_unique_key"})
	if errors.Is(other, ErrDecisionAttemptDenied) {
		t.Fatalf("unrelated unique key mapped to a deny: %v", other)
	}
}

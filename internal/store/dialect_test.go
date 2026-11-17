package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

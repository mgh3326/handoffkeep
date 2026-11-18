package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Private helpers of the spawn, thread and project files (hk 5074, porter A5).

// beginSnapshotA5 opens a REPEATABLE READ READ ONLY transaction on either
// dialect. On MySQL the isolation applies to this transaction only: the
// session default stays READ COMMITTED. It writes nothing, so it is not a
// store write transaction (no @hk_txn, no sync allocation).
func (s *Store) beginSnapshotA5(ctx context.Context) (transaction, error) {
	if s.dialect == mysqlDialect {
		return s.beginMySQLSnapshot(ctx)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, mapDriverError(err)
	}
	return pgTransaction{tx}, nil
}

// beginMySQLSnapshot opens the design §11.1 snapshot on MySQL: REPEATABLE READ
// READ ONLY with the consistent snapshot established at START, so a commit
// that lands between START and the first statement is already invisible. The
// driver's BeginTx only issues START TRANSACTION READ ONLY (its snapshot
// starts at the first read), so the statements run here on a pinned
// connection. SET TRANSACTION sets the isolation for the next transaction
// only, so the pooled connection keeps its READ-COMMITTED session default.
func (s *Store) beginMySQLSnapshot(ctx context.Context) (transaction, error) {
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		return nil, mapDriverError(err)
	}
	if _, err := conn.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		_ = conn.Close()
		return nil, mapDriverError(err)
	}
	if _, err := conn.ExecContext(ctx, `START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY`); err != nil {
		_ = conn.Close()
		return nil, mapDriverError(err)
	}
	return &sqlSnapshot{conn: conn}, nil
}

// a5Int64Args is the argument list of a `= ANY($n)` (one slice on PostgreSQL)
// or `IN (placeholders(len))` (one value each on MySQL) condition.
func a5Int64Args(s *Store, ids []int64) []any {
	if s.dialect != mysqlDialect {
		return []any{ids}
	}
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// a5ForeignTables is checked once per database and group: the tables the
// Discord thread lines read in other areas (changes B5, decisions B6). They are
// created by those areas' MySQL migrations; until then a MySQL database holds
// no change, deploy or decision record, so those line sources have nothing to
// offer and are left out of the query. Once present the answer is cached (a
// table is never dropped).
var a5ForeignTables sync.Map

type a5TablesKey struct {
	db    *sql.DB
	group string
}

// a5TablesExist reports whether every named table exists in the current
// database.
func a5TablesExist(ctx context.Context, s *Store, tx transaction, group string, names ...string) (bool, error) {
	key := a5TablesKey{s.mysqlPool, group}
	if _, ok := a5ForeignTables.Load(key); ok {
		return true, nil
	}
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN (`+placeholders(len(names))+`)`, args...).Scan(&n); err != nil {
		return false, err
	}
	if n != len(names) {
		return false, nil
	}
	a5ForeignTables.Store(key, true)
	return true, nil
}

// a5TaskThreadItemsQuery picks the MySQL line query for the tables this
// database has, and says how many decision cutoffs it binds.
func a5TaskThreadItemsQuery(ctx context.Context, s *Store, tx transaction) (string, int, error) {
	gates, err := a5TablesExist(ctx, s, tx, "thread-gates", "change_tasks", "change_gate_events", "changes", "change_findings", "change_threads")
	if err != nil {
		return "", 0, err
	}
	deploys, err := a5TablesExist(ctx, s, tx, "thread-deploys", "change_tasks", "deploy_changes", "deploys")
	if err != nil {
		return "", 0, err
	}
	decisions, err := a5TablesExist(ctx, s, tx, "thread-decisions", "decision_requests", "decision_resolutions")
	if err != nil {
		return "", 0, err
	}
	n := 0
	if decisions {
		n = 2
	}
	return taskThreadItemsMySQL(gates, deploys, decisions), n, nil
}

// a5Positional turns a statement numbered for PostgreSQL ($1, $2, ...) into
// MySQL's positional form: each $n becomes ? and the arguments are listed in
// order of appearance, repeated where a number is reused. A `$` that no digit
// follows (a JSON path such as '$') stays as it is.
func a5Positional(query string, args []any) (string, []any) {
	var out strings.Builder
	var ordered []any
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c != '$' {
			out.WriteByte(c)
			continue
		}
		j := i + 1
		n := 0
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			n = n*10 + int(query[j]-'0')
			j++
		}
		if j == i+1 {
			out.WriteByte(c)
			continue
		}
		if n < 1 || n > len(args) {
			panic(fmt.Sprintf("store: bad placeholder in %q", query))
		}
		out.WriteByte('?')
		ordered = append(ordered, args[n-1])
		i = j - 1
	}
	return out.String(), ordered
}

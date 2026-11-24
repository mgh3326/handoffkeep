package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"time"
)

// tableStatsRead is one MySQL read: the base tables with their figures, whether
// the engine statistics were read fresh, and how long the read took.
type tableStatsRead struct {
	tables  map[string][4]int64
	fresh   bool
	elapsed time.Duration
}

// readTableStatsMySQL reads every base table of the current schema. It pins one
// connection, sets information_schema_stats_expiry to 0 on it so DATA_LENGTH
// and INDEX_LENGTH are not cached, and restores the default before the
// connection returns to the pool. The figures come from one read-only
// REPEATABLE READ transaction, so every COUNT(*) sees the same snapshot. Its
// consistent reads take no row locks. The only identifiers used are the table
// names information_schema lists, quoted by quoteMySQLIdent.
func (s *Store) readTableStatsMySQL(ctx context.Context) (tableStatsRead, error) {
	var out tableStatsRead
	err := s.retryTx(ctx, func() error {
		var e error
		out, e = s.readTableStatsMySQLOnce(ctx)
		return e
	})
	return out, err
}

// readTableStatsMySQLOnce is one attempt of readTableStatsMySQL, run under
// retryTx so a retryable failure starts over on a fresh connection.
func (s *Store) readTableStatsMySQLOnce(ctx context.Context) (tableStatsRead, error) {
	started := time.Now()
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		return tableStatsRead{}, err
	}
	defer func() { _ = conn.Close() }()

	fresh := true
	if _, err = conn.ExecContext(ctx, `SET SESSION information_schema_stats_expiry = 0`); err != nil {
		// A server that refuses the setting still gets the snapshot; the
		// document records fresh=false so the estimates are not mistaken
		// for current ones.
		fresh = false
	} else {
		defer restoreStatsExpiry(conn)
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return tableStatsRead{}, err
	}
	defer func() { _ = tx.Rollback() }()

	type listed struct {
		name                  string
		est, data, indexBytes int64
	}
	var names []listed
	rows, err := tx.QueryContext(ctx, `SELECT TABLE_NAME, COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0), COALESCE(INDEX_LENGTH,0)
		FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME`)
	if err != nil {
		return tableStatsRead{}, err
	}
	for rows.Next() {
		var x listed
		if err = rows.Scan(&x.name, &x.est, &x.data, &x.indexBytes); err != nil {
			_ = rows.Close()
			return tableStatsRead{}, err
		}
		names = append(names, x)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return tableStatsRead{}, err
	}
	_ = rows.Close()

	out := tableStatsRead{tables: make(map[string][4]int64, len(names)), fresh: fresh}
	for _, x := range names {
		var exact int64
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteMySQLIdent(x.name)).Scan(&exact); err != nil {
			return tableStatsRead{}, err
		}
		out.tables[x.name] = [4]int64{exact, x.est, x.data, x.indexBytes}
	}
	out.elapsed = time.Since(started)
	return out, nil
}

// restoreStatsExpiry puts the session setting back to its default before the
// connection is reused. If that fails, the connection is discarded rather than
// returned to the pool with the changed setting.
func restoreStatsExpiry(conn *sql.Conn) {
	cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(cleanup, `SET SESSION information_schema_stats_expiry = DEFAULT`); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// quoteMySQLIdent quotes a MySQL identifier: backquotes inside it are doubled.
func quoteMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"
)

// mysqlMigration steps must be restartable: MySQL DDL commits independently,
// so the marker is written only after every step succeeds.
type mysqlMigration struct {
	id    string
	after []string
	steps []string
}

var mysqlMigrations []mysqlMigration

func registerMySQLMigration(m mysqlMigration) { mysqlMigrations = append(mysqlMigrations, m) }

func orderMySQLMigrations(migrations []mysqlMigration) ([]mysqlMigration, error) {
	resolved, err := resolveMySQLCollationMigration(migrations)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]mysqlMigration, len(resolved))
	for _, m := range resolved {
		if err := checkMigrationID(m.id); err != nil {
			return nil, err
		}
		if _, ok := byID[m.id]; ok {
			return nil, fmt.Errorf("duplicate mysql migration %s", m.id)
		}
		if len(m.steps) == 0 {
			return nil, fmt.Errorf("mysql migration %s has no steps", m.id)
		}
		byID[m.id] = m
	}
	for _, m := range resolved {
		for _, dep := range m.after {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("mysql migration %s has unknown dependency %s", m.id, dep)
			}
		}
	}
	var out []mysqlMigration
	done := map[string]bool{}
	keys := make([]string, 0, len(byID))
	for id := range byID {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for len(out) < len(keys) {
		next := ""
		for _, id := range keys {
			if done[id] {
				continue
			}
			ready := true
			for _, dep := range byID[id].after {
				if !done[dep] {
					ready = false
					break
				}
			}
			if ready {
				next = id
				break
			}
		}
		if next == "" {
			return nil, fmt.Errorf("mysql migration dependency cycle")
		}
		out = append(out, byID[next])
		done[next] = true
	}
	return out, nil
}

type mysqlSchemaReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// A current schema requires only reads: opening another process takes no DDL
// or migration lock when all this binary's migration IDs are already present.
func mysqlMigrationsCurrent(ctx context.Context, db mysqlSchemaReader, migrations []mysqlMigration) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='schema_migrations')`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM schema_migrations`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, err
		}
		applied[id] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, m := range migrations {
		if !applied[m.id] {
			return false, nil
		}
	}
	return true, nil
}

// migrateMySQL serializes schema changes on one pinned connection. An aborted
// run leaves its marker absent, and its idempotent steps run again on restart.
func migrateMySQL(ctx context.Context, db *sql.DB, migrations []mysqlMigration) error {
	ordered, err := orderMySQLMigrations(migrations)
	if err != nil {
		return err
	}
	current, err := mysqlMigrationsCurrent(ctx, db, ordered)
	if err != nil || current {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(CONCAT('hk:migrate:', LEFT(SHA2(DATABASE(), 256), 48)), 30)`).Scan(&acquired); err != nil {
		// An interrupted GET_LOCK may have acquired the lock before its result was
		// lost. Closing the physical connection is the only safe cleanup.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("acquire mysql migration lock")
	}
	defer func() {
		var released sql.NullInt64
		err := conn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(CONCAT('hk:migrate:', LEFT(SHA2(DATABASE(), 256), 48)))`).Scan(&released)
		if err != nil || !released.Valid || released.Int64 != 1 {
			// Do not return a connection that could still own the lock to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	current, err = mysqlMigrationsCurrent(ctx, conn, ordered)
	if err != nil || current {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (id VARCHAR(64) NOT NULL PRIMARY KEY) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`); err != nil {
		return err
	}
	for _, m := range ordered {
		var applied bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=?)`, m.id).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		for i, step := range m.steps {
			if err := execMySQLStep(ctx, conn, step); err != nil {
				return fmt.Errorf("mysql migration %s step %d: %w", m.id, i+1, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(id) VALUES (?)`, m.id); err != nil {
			return err
		}
	}
	return nil
}

// MySQL has no ADD COLUMN, CREATE INDEX or ADD CONSTRAINT IF NOT EXISTS, and a
// migration's DDL commits step by step, so a step that adds one of them must
// check the catalog itself to be restartable. These constructors wrap a step
// with the object it creates; the runner skips the step when the object is
// already there. The guard checks one object, so a guarded ALTER that adds
// several columns is guarded by its first.
const mysqlGuardSep = "\x00"

func mysqlIfNoColumn(table, column, stmt string) string {
	return strings.Join([]string{"", "column", table, column, stmt}, mysqlGuardSep)
}
func mysqlIfColumnNotNull(table, column, stmt string) string {
	return strings.Join([]string{"", "not_null", table, column, stmt}, mysqlGuardSep)
}
func mysqlIfNoIndex(table, index, stmt string) string {
	return strings.Join([]string{"", "index", table, index, stmt}, mysqlGuardSep)
}
func mysqlIfNoConstraint(table, name, stmt string) string {
	return strings.Join([]string{"", "constraint", table, name, stmt}, mysqlGuardSep)
}

// mysqlIfConstraint runs the step only while the named constraint exists, so a
// DROP is restartable.
func mysqlIfConstraint(table, name, stmt string) string {
	return strings.Join([]string{"", "has_constraint", table, name, stmt}, mysqlGuardSep)
}

// mysqlRequireEmpty is a refusal, not a conditional: the step fails with the
// table's name and row count while the table holds any row, and passes when it
// is empty or absent (requireMySQLEmpty). It has no statement of its own.
func mysqlRequireEmpty(table string) string {
	return strings.Join([]string{"", "require_empty", table, table, ""}, mysqlGuardSep)
}

func parseMySQLGuard(step string) (kind, table, object, stmt string, ok bool) {
	if !strings.HasPrefix(step, mysqlGuardSep) {
		return "", "", "", step, false
	}
	parts := strings.SplitN(step, mysqlGuardSep, 5)
	if len(parts) != 5 {
		return "", "", "", step, false
	}
	return parts[1], parts[2], parts[3], parts[4], true
}

func execMySQLStep(ctx context.Context, conn *sql.Conn, step string) error {
	kind, table, object, stmt, guarded := parseMySQLGuard(step)
	if !guarded {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	}
	if kind == "require_empty" {
		return requireMySQLEmpty(ctx, conn, table)
	}
	var query string
	switch kind {
	case "column":
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?)`
	case "not_null":
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=? AND is_nullable='NO')`
	case "index":
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?)`
	case "constraint", "has_constraint":
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.table_constraints WHERE table_schema=DATABASE() AND table_name=? AND constraint_name=?)`
	default:
		return fmt.Errorf("unknown mysql step guard %q", kind)
	}
	var exists bool
	if err := conn.QueryRowContext(ctx, query, table, object).Scan(&exists); err != nil {
		return err
	}
	if !shouldRunMySQLGuard(kind, exists) {
		return nil
	}
	_, err := conn.ExecContext(ctx, stmt)
	return err
}

// requireMySQLEmpty fails, naming table and its row count, while table holds a
// row. A table that does not exist passes. The table name is a code constant,
// so the count query interpolates it.
func requireMySQLEmpty(ctx context.Context, conn *sql.Conn, table string) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?)`, table).Scan(&exists); err != nil || !exists {
		return err
	}
	var rows int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&rows); err != nil {
		return err
	}
	if rows > 0 {
		return fmt.Errorf("%s holds %d rows; it is dropped only when empty", table, rows)
	}
	return nil
}

// shouldRunMySQLGuard interprets the catalog predicate for a guarded step.
// not_null reports true when a nullable conversion still needs to run;
// has_constraint runs while the constraint is still there.
func shouldRunMySQLGuard(kind string, exists bool) bool {
	if kind == "not_null" || kind == "has_constraint" {
		return exists
	}
	return !exists
}

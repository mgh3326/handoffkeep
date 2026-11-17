package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sort"
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
	byID := make(map[string]mysqlMigration, len(migrations))
	for _, m := range migrations {
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
	for _, m := range migrations {
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

// migrateMySQL serializes schema changes on one pinned connection. An aborted
// run leaves its marker absent, and its idempotent steps run again on restart.
func migrateMySQL(ctx context.Context, db *sql.DB, migrations []mysqlMigration) error {
	ordered, err := orderMySQLMigrations(migrations)
	if err != nil {
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
			if _, err := conn.ExecContext(ctx, step); err != nil {
				return fmt.Errorf("mysql migration %s step %d: %w", m.id, i+1, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(id) VALUES (?)`, m.id); err != nil {
			return err
		}
	}
	return nil
}

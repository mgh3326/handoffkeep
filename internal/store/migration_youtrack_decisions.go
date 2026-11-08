package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261108-youtrack-decisions adds the decision mirror the Go YouTrack sync
// keeps: one row per decision request whose DEC issue it created or adopted.
// It is a new table with no foreign key, so it locks no existing table, and
// the migration writes no row. A decision with no row is found by its 결정 ID
// field before a create, so the table is an index, not the only identity.
func init() {
	registerMigration(migration{ID: "20261108-youtrack-decisions", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS youtrack_decision_mirrors (
				decision_id BIGINT PRIMARY KEY,
				issue_id TEXT NOT NULL UNIQUE,
				display_ref TEXT NOT NULL DEFAULT '',
				stamp TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		})
	}})
}

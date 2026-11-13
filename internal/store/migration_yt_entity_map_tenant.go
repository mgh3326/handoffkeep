package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-yt-entity-map-tenant scopes the YouTrack ids in yt_entity_map to
// the tenant that holds them (hk 3912). YouTrack ids are per instance: issue
// "2-1" on one tenant and "2-1" on another are different issues. The map's
// unique index covered (entity_kind, yt_id) with no tenant, so the second
// tenant's row was ErrConflictingResult for an op whose create had already
// succeeded remotely, and the binding that apply publishes was never written.
//
// Expand-only: one column with a default (a previous release inserts its rows
// without it and gets ”), a backfill from the op that wrote each row (every
// row names its op, and the op carries its tenant), and a wider unique index
// replacing the old constraint. The previous release reads the map by hk_key
// (unchanged) and checks conflicts by yt_id; among the rows it writes (all
// ”) the new index enforces what the old one did.
//
// Locks: ADD COLUMN takes ACCESS EXCLUSIVE on yt_entity_map for the catalog
// change only (no rewrite: the default fills existing rows), the UPDATE takes
// the table's row locks, DROP CONSTRAINT and CREATE UNIQUE INDEX take SHARE.
// No other table is written. An Apply that inserts a map row waits for the
// ACCESS EXCLUSIVE lock and then runs against the new shape.
//
// After names the migration that creates the table and the op log this one
// reads, the dependency its statements have.
const YTEntityMapTenantMigration = "20261113-yt-entity-map-tenant"

func init() {
	registerMigration(migration{ID: YTEntityMapTenantMigration, After: []string{"20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytEntityMapTenantStatements)
	}})
}

var ytEntityMapTenantStatements = []string{
	`ALTER TABLE yt_entity_map ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '' CHECK (length(tenant) <= 255)`,
	`UPDATE yt_entity_map m SET tenant = o.tenant FROM yt_write_ops o WHERE o.id = m.op_id AND m.tenant = ''`,
	`ALTER TABLE yt_entity_map DROP CONSTRAINT IF EXISTS yt_entity_map_entity_kind_yt_id_key`,
	`CREATE UNIQUE INDEX IF NOT EXISTS yt_entity_map_entity_kind_tenant_yt_id_key ON yt_entity_map(entity_kind, tenant, yt_id)`,
}

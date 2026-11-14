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
// Locks: ADD COLUMN takes ACCESS EXCLUSIVE on yt_entity_map (no rewrite: the
// default fills existing rows), and so does DROP CONSTRAINT. The runner
// applies the entry in one transaction (migrateRegistered), so that lock is
// held through the backfill UPDATE and the CREATE UNIQUE INDEX build until
// commit: every read and write of yt_entity_map, the previous release's
// included, waits for the migration. The table holds one row per mapped
// entity, so this is a short stall, not an outage. yt_write_ops is only read
// (ACCESS SHARE); no other table is written. An Apply that inserts a map row
// waits for the lock and then runs against the new shape.
//
// After names the migration that creates the table and the op log this one
// reads, and the one that adds yt_write_ops.tenant, which the backfill reads.
const YTEntityMapTenantMigration = "20261113-yt-entity-map-tenant"

func init() {
	registerMigration(migration{ID: YTEntityMapTenantMigration, After: []string{"20261109-yt-write-ops", "20261109-yt-comment-canary"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytEntityMapTenantStatements)
	}})
}

var ytEntityMapTenantStatements = []string{
	`ALTER TABLE yt_entity_map ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '' CHECK (length(tenant) <= 255)`,
	`UPDATE yt_entity_map m SET tenant = o.tenant FROM yt_write_ops o WHERE o.id = m.op_id AND m.tenant = ''`,
	`ALTER TABLE yt_entity_map DROP CONSTRAINT IF EXISTS yt_entity_map_entity_kind_yt_id_key`,
	`CREATE UNIQUE INDEX IF NOT EXISTS yt_entity_map_entity_kind_tenant_yt_id_key ON yt_entity_map(entity_kind, tenant, yt_id)`,
}

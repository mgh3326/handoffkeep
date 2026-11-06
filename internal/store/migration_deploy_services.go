package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// The console needs a registry of deploy-record prefixes independent of deploy
// attempts. Creating and seeding this new table takes ACCESS EXCLUSIVE on the
// new table only; no existing table is locked.
func init() {
	registerMigration(migration{ID: "20261107-deploy-services", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE deploy_services (name TEXT PRIMARY KEY, repo TEXT NOT NULL, record_prefixes TEXT[] NOT NULL, active BOOLEAN NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
			`INSERT INTO deploy_services (name,repo,record_prefixes,active,created_at,updated_at) VALUES
   ('handoffkeep','mgh3326/handoffkeep',ARRAY['deploy/handoffkeep/'],true,now(),now()),
   ('auto_trader','mgh3326/auto_trader',ARRAY['deploy/auto_trader/'],true,now(),now()),
   ('panewire-hub','mgh3326/panewire',ARRAY['deploy/panewire-hub/','deploy/panewire/'],true,now(),now()),
   ('mergewake','mgh3326/mergewake',ARRAY['deploy/mergewake/'],true,now(),now()),
   ('pacelever','mgh3326/pacelever',ARRAY['deploy/pacelever/'],true,now(),now())`,
		})
	}})
}

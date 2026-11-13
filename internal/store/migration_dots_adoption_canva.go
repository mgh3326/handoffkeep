package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const dotsAdoptionCanvaMigrationID = "20261113-dots-adoption-canva"

func init() {
	registerMigration(migration{ID: dotsAdoptionCanvaMigrationID, Apply: applyDotsAdoptionCanvaMigration})
}

// applyDotsAdoptionCanvaMigration adds the Canva source of an adopted dots
// result: the design's link, page and version (hk:doc policy/dots-on-hk). The
// columns are nullable with no default, so existing rows read as empty and the
// previous release, which never reads them, keeps working. ADD COLUMN without
// a default takes ACCESS EXCLUSIVE on dots_results only for the catalog change,
// a short wait at startup.
func applyDotsAdoptionCanvaMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, dotsAdoptionCanvaMigration)
}

var dotsAdoptionCanvaMigration = []string{
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS canva_url TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS canva_page TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS canva_version TEXT`,
}

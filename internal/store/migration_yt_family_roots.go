package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261115-yt-family-roots records the root article hk resolved for one family
// on one YouTrack project (hk 4797). A family root is not an op — no op row can
// hold its id — so before this table every document of the family searched for
// the root again, and a root POST that timed out was retried as not_sent: the
// retry's search saw nothing yet and made a second root beside the one the
// first POST had created (30 "briefs" roots and 28 "brief" in production). The
// row is written by the article_create send that resolved the root
// (Ledger.RecordFamilyRoot) and read by every later document of the family
// (Ledger.FamilyRoot), which then needs no search.
//
// Expand-only: a new table nothing else writes, so the previous release, which
// never reads or writes it, keeps serving during blue-green.
//
// Locks: the table is new, so CREATE TABLE takes no lock on an existing table
// and blocks no writer.
const YTFamilyRootsMigration = "20261115-yt-family-roots"

func init() {
	registerMigration(migration{ID: YTFamilyRootsMigration, After: []string{"20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytFamilyRootsStatements)
	}})
}

var ytFamilyRootsStatements = []string{
	// One root per family per project: the article every document of the family
	// goes under. op_id is the article_create op whose dispatch resolved it, so
	// the record can be traced to the write that made it.
	`CREATE TABLE IF NOT EXISTS yt_family_roots (
		tenant TEXT NOT NULL DEFAULT '' CHECK (length(tenant) <= 255),
		project_id TEXT NOT NULL CHECK (project_id <> '' AND length(project_id) <= 64),
		family TEXT NOT NULL CHECK (family <> '' AND length(family) <= 64),
		article_id TEXT NOT NULL CHECK (article_id <> '' AND length(article_id) <= 64),
		op_id BIGINT NOT NULL REFERENCES yt_write_ops(id),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (tenant, project_id, family)
	)`,
}

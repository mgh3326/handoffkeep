package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTAuthorityMigration (hk 4122, docs/youtrack-write-protocol.md §Authority)
// adds yt_authority: for each content kind, which side is canonical, hk (PG)
// or YouTrack. The switch (Y8) is one transaction over these rows
// (SetYTAuthority), so it is recorded in the database rather than in an env
// value an older release cannot parse, and a rollback is setting a row back
// to hk.
//
// Expand-only: one new table seeded with one 'hk' row per kind, which is
// today's behaviour. It touches no existing table and takes no lock on one;
// the previous release neither reads nor writes it. A kind with no row reads
// as hk (ytAuthorityTx).
const YTAuthorityMigration = "20261112-yt-authority"

func init() {
	registerMigration(migration{ID: YTAuthorityMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytAuthorityStatements)
	}})
}

var ytAuthorityStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_authority (
		kind TEXT PRIMARY KEY CHECK(kind IN ('tasks','comments','documents','decisions')),
		authority TEXT NOT NULL DEFAULT 'hk' CHECK(authority IN ('hk','youtrack')),
		switched_at TIMESTAMPTZ,
		switched_by TEXT NOT NULL DEFAULT '' CHECK(length(switched_by) <= 200),
		note TEXT NOT NULL DEFAULT '' CHECK(length(note) <= 2000))`,
	`INSERT INTO yt_authority(kind) VALUES ('tasks'),('comments'),('documents'),('decisions') ON CONFLICT DO NOTHING`,
}

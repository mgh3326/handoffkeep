package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261110-yt-comments-on adds what slice b1 (hk 3810) needs for mode 'on':
//
//   - yt_entity_map.op_id becomes nullable, so tasks verified by CLI or worker
//     before any write op exists can be recorded into yt_entity_map without an op.
//   - yt_entity_map.tenant: the YouTrack host where the entity was verified,
//     used to bind comment ops to the verified tenant when comments are created.
//
// Expand-only: ALTER COLUMN op_id DROP NOT NULL widens allowed values; ADD
// COLUMN IF NOT EXISTS tenant adds a column with a constant default ”.
// Locks: ACCESS EXCLUSIVE on yt_entity_map briefly. yt_entity_map holds
// minimal rows, so the lock completes instantaneously.
func init() {
	registerMigration(migration{
		ID:    "20261110-yt-comments-on",
		After: []string{"20261109-yt-comment-canary"},
		Apply: func(ctx context.Context, tx pgx.Tx) error {
			return execAll(ctx, tx, ytCommentsOnStatements)
		},
	})
}

var ytCommentsOnStatements = []string{
	`ALTER TABLE yt_entity_map ALTER COLUMN op_id DROP NOT NULL`,
	`ALTER TABLE yt_entity_map ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '' CHECK(length(tenant) <= 255)`,
}

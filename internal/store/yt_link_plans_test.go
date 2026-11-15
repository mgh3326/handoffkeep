package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// TestYTLinksProtocolOverPostgres runs the issue_links contract (hk 4638 S3)
// over the Postgres ledger. One schema serves every subtest: yt_link_plans is
// append-only (its trigger refuses TRUNCATE), so op ids keep counting up
// instead of restarting, and an old plan row can never name a new op.
func TestYTLinksProtocolOverPostgres(t *testing.T) {
	s := schemaStore(t, "ytlp")
	ytwritetest.RunLinksProtocol(t, func(t *testing.T) ytwrite.Ledger {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(), `TRUNCATE yt_link_owned, yt_link_unowned, yt_link_steps, yt_comment_write_links, yt_entity_map, yt_write_ops, yt_write_op_adoptions, yt_issue_comment_bounds`); err != nil {
			t.Fatal(err)
		}
		return s.YTWriteLedger()
	})
}

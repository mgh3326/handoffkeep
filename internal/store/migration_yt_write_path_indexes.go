package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261116-yt-write-path-indexes ends the whole-table scans the YouTrack
// write worker's every-five-second pass made (hk 4922; production
// 2026-11-04 to 2026-11-16, 959,016 sequential scans of yt_article_bindings
// reading 13.85 billion rows, 1,847,809 of yt_comment_write_links reading
// 11.6 billion, and 11.68 billion index entries read from yt_write_ops):
//
//   - yt_article_bindings_document_text. The article_create gate
//     (ytArticleCreateGateSQL) compares the op's text entity id with the
//     binding's bigint document id as `b.document_id::text = o.entity_id`.
//     The cast is on the indexed column, and the gate names no other column
//     of the table (the article_update gate's tenant and article id are the
//     unique key, which serves it), so every evaluation scanned all 15,657
//     bindings. The gate is read for every prepared article op a pass lists
//     (Due), claims (Claim) and examines, so the scan ran about once per
//     pass. The expression index serves the comparison exactly as written.
//   - yt_write_ops_comment_requeue. The moved-comment requeue's candidate
//     scan (ytRequeueSelectSQL) filters on kind, state, reason, yt_target_id
//     and attempts, none of which an index served, so a pass read its whole
//     comment family (every comment_append op, 11.68 billion index entries
//     over twelve days) and joined yt_comment_write_links under it. The
//     partial index holds only the ops a pass can copy again, which is
//     usually none, so the scan starts at an empty index and the joins under
//     it are never entered. Its predicate names the same kind, state, reason,
//     empty target and attempt count the query writes as literals
//     (ytwrite.KindCommentAppend, ytwrite.StateRejected,
//     ytwrite.ReasonAmbiguousIssue), so a plan of either kind proves it
//     applies (hk 4922).
//
// After 20261112-yt-article-bindings and 20261109-yt-comment-canary, which
// adds yt_write_ops.tenant: the second index names it.
//
// Expand-only: two indexes, no column, table or constraint change, and the
// previous release reads and writes both tables exactly as before. Locks:
// CREATE INDEX takes SHARE on its table and holds back writes to it for the
// build, until the migrating transaction commits. yt_article_bindings holds
// 15,657 rows and yt_write_ops 28,055 on production, and the second index is
// partial (an empty candidate set, a handful of rows at most), so both builds
// are a short scan of a small table. The migration writes no row.
func init() {
	registerMigration(migration{ID: YTWritePathIndexesMigration, After: []string{YTArticleBindingsMigration, "20261109-yt-comment-canary"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytWritePathIndexStatements)
	}})
}

// YTWritePathIndexesMigration is the migration's registered ID.
const YTWritePathIndexesMigration = "20261116-yt-write-path-indexes"

var ytWritePathIndexStatements = []string{
	`CREATE INDEX IF NOT EXISTS yt_article_bindings_document_text ON yt_article_bindings ((document_id::text))`,
	`CREATE INDEX IF NOT EXISTS yt_write_ops_comment_requeue ON yt_write_ops (tenant, id)
		WHERE kind = 'comment_append' AND state = 'rejected' AND reason = 'ambiguous_issue'
			AND yt_target_id = '' AND attempts = 0`,
}

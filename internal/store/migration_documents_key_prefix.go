package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261116-documents-key-prefix replaces documents_prefix with an index a
// prefix read can actually use.
//
// documents_prefix is a btree on the key with the default operator class and
// the database collation, which is en_US.UTF-8. A prefix is not a range in that
// order, and the planner refuses the parameterized prefix reads (ListDocuments,
// the deploy-record scans): the LIKE form needs a constant pattern, and the
// range form needs an index whose comparison order is byte order. Every such
// read fell back to a sequential scan of the whole table — 171,534 scans and
// 2.84 billion rows in twelve days (hk 4923).
//
// documents_key_c is a btree on the key under the "C" collation, i.e. byte
// order. The half-open range in keyPrefixBounds is then exactly the set of keys
// that start with a prefix, and the planner scans the index for a prefix
// supplied as a parameter. The same collation also serves the ORDER BY of the
// cursor-paged reads, so a page is an ordered index scan of the page size
// instead of a sort of the whole prefix range. The rewrite of those reads is in
// the same release.
//
// Expand-only: this adds one index and drops one that no release names. An
// index is a plan, not a contract, and the previous release's reads return the
// same rows without either. Locks: CREATE INDEX takes SHARE on documents for
// the build, which holds back the previous release's document writes for its
// duration (33k keys, body heap untouched by a key-only index: seconds on the
// target hardware). DROP INDEX takes ACCESS EXCLUSIVE on documents, held from
// the drop until commit, so it queues behind the running queries and then
// blocks reads and writes until the commit. The migration writes no row.
func init() {
	registerMigration(migration{ID: DocumentsKeyPrefixMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, documentsKeyPrefixStatements)
	}})
}

// DocumentsKeyPrefixMigration is the migration's registered ID.
const DocumentsKeyPrefixMigration = "20261116-documents-key-prefix"

var documentsKeyPrefixStatements = []string{
	`CREATE INDEX IF NOT EXISTS documents_key_c ON documents ((key COLLATE "C"))`,
	`DROP INDEX IF EXISTS documents_prefix`,
}

package store

// Version 52 adds the review-history fields mergewake reads back from hk
// (hk 3208, requested on hk 3176) and the delivery id of a checks write.
//
// reviews gains reviewer, same_family, delta, quiet, outside_diff_blocking and
// patch_digest, all nullable: NULL means unknown, never false or zero. The
// columns are new because reviews held only the hex patch id, which is not
// enough to judge patch inheritance or reviewer independence; patch_digest is
// the digest half of mergewake's patch receipt "<patch-id>:<digest>" and
// patch_id stays as it is. change_checks gains delivery_id, the webhook
// delivery that last wrote the check row, so hk 3187 can measure missed check
// webhooks.
//
// Every statement is ADD COLUMN IF NOT EXISTS without a volatile default: a
// catalog-only change, no table rewrite. Each takes ACCESS EXCLUSIVE on
// reviews or change_checks for the instant of the catalog update, and the
// CHECK on outside_diff_blocking has no existing rows to validate against
// (the column is new and NULL).
var reviewHistoryV52 = []string{
	`ALTER TABLE reviews
		ADD COLUMN IF NOT EXISTS reviewer TEXT,
		ADD COLUMN IF NOT EXISTS same_family BOOLEAN,
		ADD COLUMN IF NOT EXISTS delta BOOLEAN,
		ADD COLUMN IF NOT EXISTS quiet BOOLEAN,
		ADD COLUMN IF NOT EXISTS outside_diff_blocking INT CHECK(outside_diff_blocking >= 0),
		ADD COLUMN IF NOT EXISTS patch_digest TEXT`,
	`ALTER TABLE change_checks ADD COLUMN IF NOT EXISTS delivery_id TEXT`,
	`INSERT INTO schema_version(version) VALUES (52)`,
}

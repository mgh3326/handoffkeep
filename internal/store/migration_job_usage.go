package store

// Version 39 adds a job's token usage (docs: hk:doc
// handoff/2026-11-05/handoffkeep-design-usage-outcome-contract, hk 3005):
// five nullable token counts, the harness record they came from, and when
// they were written. NULL means unknown, never zero; panewire writes them at
// completion or reap (UpdateJob, JobUsage).
//
// Each statement adds a nullable column without a default, a catalog-only
// change: it takes the jobs table's ACCESS EXCLUSIVE lock for the instant of
// the catalog update and rewrites no row.
var jobUsageV39 = []string{
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS input_tokens BIGINT CHECK(input_tokens >= 0)`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS output_tokens BIGINT CHECK(output_tokens >= 0)`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS reasoning_tokens BIGINT CHECK(reasoning_tokens >= 0)`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT CHECK(cache_read_tokens >= 0)`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT CHECK(cache_write_tokens >= 0)`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS usage_source TEXT`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS usage_recorded_at TIMESTAMPTZ`,
	`INSERT INTO schema_version(version) VALUES (39)`,
}

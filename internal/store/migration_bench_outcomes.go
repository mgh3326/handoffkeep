package store

// Version 40 gives bench_reps first-class experiment and outcome columns
// (hk 2886; contract hk:doc
// handoff/2026-11-05/handoffkeep-design-usage-outcome-contract §2). Each is
// nullable and NULL means unknown, never zero; late outcomes arrive through
// the set-once PatchBenchRepOutcome (bench_outcomes.go). experiment_id has no
// foreign key until the experiment registry (hk 2885) exists.
//
// Each statement adds a nullable column without a default under the
// bench_reps ACCESS EXCLUSIVE lock; a column CHECK validates existing rows in
// the same scan, which every row passes with NULL (about 2,200 rows today).
var benchOutcomesV40 = []string{
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS experiment_id TEXT`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS arm TEXT CHECK(arm IN ('baseline','arm'))`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS first_ci_green BOOLEAN`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS critical_findings INT CHECK(critical_findings >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS high_findings INT CHECK(high_findings >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS merged BOOLEAN`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS reverted_within_7d BOOLEAN`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS reviewer_profile TEXT`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS reviewer_effort TEXT`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS reasoning_tokens BIGINT CHECK(reasoning_tokens >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT CHECK(cache_read_tokens >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT CHECK(cache_write_tokens >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS spawn_to_ready_s INT CHECK(spawn_to_ready_s >= 0)`,
	`ALTER TABLE bench_reps ADD COLUMN IF NOT EXISTS spawn_to_merge_s INT CHECK(spawn_to_merge_s >= 0)`,
	`INSERT INTO schema_version(version) VALUES (40)`,
}

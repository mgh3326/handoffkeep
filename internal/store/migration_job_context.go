package store

// Version 43 records job context and permits jobs without a task. Each ALTER
// takes ACCESS EXCLUSIVE on jobs until commit. The nullable ADD COLUMNs and
// DROP NOT NULL are catalog-only and rewrite no row. Column CHECKs validate
// existing rows in one scan under that lock; NULL passes each check. ClaimJob
// and UpdateJob wait for the duration.
var jobContextV43 = []string{
	`ALTER TABLE jobs
		ADD COLUMN harness TEXT,
		ADD COLUMN effort TEXT,
		ADD COLUMN claimed_at TIMESTAMPTZ,
		ADD COLUMN spawned_at TIMESTAMPTZ,
		ADD COLUMN spawned_received_at TIMESTAMPTZ,
		ADD COLUMN ready_at TIMESTAMPTZ,
		ADD COLUMN ready_received_at TIMESTAMPTZ,
		ADD COLUMN ended_at TIMESTAMPTZ,
		ADD COLUMN ended_received_at TIMESTAMPTZ,
		ADD COLUMN report_sha256 TEXT CHECK(report_sha256 ~ '^[0-9a-f]{64}$'),
		ADD COLUMN report_version INT CHECK(report_version >= 1),
		ADD COLUMN skills JSONB CHECK(jsonb_typeof(skills) = 'array'),
		ADD COLUMN skills_supported BOOLEAN,
		ADD COLUMN end_reason TEXT,
		ADD COLUMN diff_files INT CHECK(diff_files >= 0),
		ADD COLUMN diff_insertions INT CHECK(diff_insertions >= 0),
		ADD COLUMN diff_deletions INT CHECK(diff_deletions >= 0)`,
	`ALTER TABLE jobs ALTER COLUMN task_id DROP NOT NULL`,
	`INSERT INTO schema_version(version) VALUES (43)`,
}

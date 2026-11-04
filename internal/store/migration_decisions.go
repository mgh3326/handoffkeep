package store

// Version 35 adds decision_requests: one row per question put to the
// operator, whatever source holds it (docs/linear-internalize.md, "Decision
// requests"). The source's own record stays the authority; the row mirrors
// it and is written in the source writer's transaction (decision_records.go).
// It is a new table with its own indexes, so it locks no existing table, and
// it creates no row: questions that were open before this version get no row
// (the dry run, decision_backfill.go, reports them instead).
//
// status 'expired' is accepted by the constraint but never stored: a passed
// expires_at changes how an open row is listed, not what it records.
var decisionsV35 = []string{
	`CREATE TABLE IF NOT EXISTS decision_requests (
		id BIGSERIAL PRIMARY KEY,
		source TEXT NOT NULL CHECK(source IN ('task_flag','task_request','job_escalate','lane_event','chat','linear')),
		source_ref TEXT NOT NULL CHECK(source_ref <> ''),
		task_id BIGINT,
		job_id TEXT NOT NULL DEFAULT '',
		lane TEXT NOT NULL DEFAULT '',
		question TEXT NOT NULL DEFAULT '',
		options JSONB,
		recommended TEXT NOT NULL DEFAULT '',
		default_action TEXT NOT NULL DEFAULT '',
		due_at TIMESTAMPTZ,
		snoozed_until TIMESTAMPTZ,
		expires_at TIMESTAMPTZ,
		status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','answered','superseded','expired')),
		answer TEXT NOT NULL DEFAULT '',
		answered_by TEXT NOT NULL DEFAULT '',
		answered_at TIMESTAMPTZ,
		superseded_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		UNIQUE(source, source_ref)
	)`,
	`CREATE INDEX IF NOT EXISTS decision_requests_task_open ON decision_requests(task_id) WHERE status='open' AND task_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS decision_requests_job_open ON decision_requests(job_id) WHERE status='open' AND job_id<>''`,
	`CREATE INDEX IF NOT EXISTS decision_requests_status_created ON decision_requests(status, created_at, id)`,
	`INSERT INTO schema_version(version) VALUES (35)`,
}

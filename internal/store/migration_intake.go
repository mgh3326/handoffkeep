package store

// Version 36 adds triage intake (docs/linear-internalize.md, "Triage
// intake"):
//
//   - tasks.source and tasks.source_ref name the producer of a task that came
//     in through intake or a ticket set. The partial unique index makes a
//     repeated (source, source_ref) return the existing task instead of a
//     second one. Rows created by tasks add leave both empty and are outside
//     the index.
//   - ticket_sets records one emitted set per (source, source_ref) with the
//     hash of its payload, so the same payload returns the same ids and a
//     changed one is refused (Q12).
//   - the empty kind is the unset kind. A task holds it in triage, and keeps
//     it when it is declined from triage (dropped or duplicate); the triage
//     gate refuses an accept without a kind, the constraint backs it, and a
//     kind-less declined task cannot be reopened.
//
// ADD COLUMN with a constant default and the constraint swap take a brief
// ACCESS EXCLUSIVE lock on tasks; the constraint is replaced NOT VALID, so
// existing rows (all of which carry a kind) are not scanned. CREATE INDEX
// takes a SHARE lock on tasks while it builds. ticket_sets is a new table.
var intakeV36 = []string{
	`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS source_ref TEXT NOT NULL DEFAULT ''`,
	`CREATE UNIQUE INDEX IF NOT EXISTS tasks_source_ref ON tasks(source, source_ref) WHERE source<>'' AND source_ref<>''`,
	`ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_kind_check, ADD CONSTRAINT tasks_kind_check CHECK(kind IN ('implement','verify','fix','decide','ops') OR (kind='' AND state IN ('triage','dropped','duplicate'))) NOT VALID`,
	`CREATE TABLE IF NOT EXISTS ticket_sets (
		id BIGSERIAL PRIMARY KEY,
		source TEXT NOT NULL CHECK(source <> ''),
		source_ref TEXT NOT NULL CHECK(source_ref <> ''),
		payload_sha256 TEXT NOT NULL CHECK(payload_sha256 ~ '^[0-9a-f]{64}$'),
		parent_task_id BIGINT NOT NULL UNIQUE REFERENCES tasks(id),
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE(source, source_ref)
	)`,
	`INSERT INTO schema_version(version) VALUES (36)`,
}

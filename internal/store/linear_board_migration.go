package store

// Version 29 adds the placement audit log and the work-board projection. The
// ALTER on linear_outbox takes ACCESS EXCLUSIVE briefly; foreign keys take
// SHARE ROW EXCLUSIVE on tasks. No historic rows are rewritten.
var linearBoardV29 = []string{
	`CREATE TABLE placement_decisions (
 id BIGSERIAL PRIMARY KEY, job_id TEXT NOT NULL, attempt INT NOT NULL DEFAULT 1 CHECK(attempt>0),
 task_id BIGINT, decided_at TIMESTAMPTZ NOT NULL, decided_on TEXT NOT NULL,
 tool_version TEXT NOT NULL DEFAULT '', source TEXT NOT NULL CHECK(source IN ('auto','forced')),
 ranking TEXT NOT NULL, state_source TEXT NOT NULL, chosen_host TEXT,
 outcome TEXT NOT NULL CHECK(outcome IN ('placed','refused','failed')), reason TEXT NOT NULL DEFAULT '',
 profile TEXT NOT NULL DEFAULT '', pool TEXT NOT NULL DEFAULT '', account_key TEXT NOT NULL DEFAULT '',
 family TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '',
 pane_id TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', candidates JSONB NOT NULL,
 UNIQUE(job_id,attempt))`,
	`CREATE INDEX placement_decisions_at ON placement_decisions(decided_at DESC,id DESC)`,
	`CREATE INDEX placement_decisions_host ON placement_decisions(chosen_host,decided_at DESC,id DESC)`,
	`CREATE INDEX placement_decisions_task ON placement_decisions(task_id,decided_at,id)`,
	`CREATE TABLE linear_board_tasks(task_id BIGINT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,status JSONB NOT NULL)`,
	`ALTER TABLE linear_outbox DROP CONSTRAINT linear_outbox_op_check`,
	`ALTER TABLE linear_outbox ADD CONSTRAINT linear_outbox_op_check CHECK(op IN ('issue_create','issue_state','terminal_comment','terminal_archive','work_board'))`,
	`INSERT INTO schema_version(version) VALUES (29)`,
}

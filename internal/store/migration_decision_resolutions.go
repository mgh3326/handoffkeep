package store

// Version 41 gives a structured decision answer a receipt (hk 3061; design
// hk:doc design/2026-11-05/hk-work-record-design-v1.1 §10.2, §13.1 R1/R2).
// Until now a dr- request was closed by writing tasks.refs.decision_request
// alone: no row said who tried to answer, through which channel, or how the
// answer was verified. Version 41 adds two tables that Store.ResolveDecision
// writes in the answer's own transaction:
//
//   - decision_answer_attempts: one append-only row per attempt, whatever its
//     result (answered, conflict, stale_revision, pending_confirmation,
//     expired, superseded, refused). UNIQUE(channel, event_key) makes a
//     retried delivery recognisable; the trigger makes append-only structural.
//   - decision_resolutions: at most one row per logical request and question
//     revision (UNIQUE(request_id, question_revision)), linked to the attempt
//     that produced it, with the provenance derived from the verified channel.
//
// Lock: both tables are new, so the statements lock nothing that exists
// except the foreign keys, which take SHARE ROW EXCLUSIVE on tasks (and on
// decision_answer_attempts for the second table) for the length of this
// transaction. No row is read or rewritten; requests answered before the
// upgrade keep their answer in tasks.refs only.
var decisionResolutionsV41 = []string{
	`CREATE TABLE IF NOT EXISTS decision_answer_attempts (
		id BIGSERIAL PRIMARY KEY,
		channel TEXT NOT NULL CHECK(channel IN ('cli','assistant','console','linear','lane','default')),
		event_key TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		request_id TEXT,
		revision_request_id TEXT NOT NULL,
		question_revision INT,
		provenance TEXT,
		actor TEXT NOT NULL,
		result TEXT NOT NULL CHECK(result IN ('answered','conflict','stale_revision','pending_confirmation','expired','superseded','refused')),
		event_at TIMESTAMPTZ,
		received_at TIMESTAMPTZ NOT NULL,
		UNIQUE(channel, event_key)
	)`,
	`CREATE OR REPLACE FUNCTION handoffkeep_decision_answer_attempts_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decision_answer_attempts is append-only'; RETURN NULL; END; $$`,
	`DROP TRIGGER IF EXISTS decision_answer_attempts_append_only ON decision_answer_attempts`,
	`CREATE TRIGGER decision_answer_attempts_append_only BEFORE UPDATE OR DELETE ON decision_answer_attempts FOR EACH ROW EXECUTE FUNCTION handoffkeep_decision_answer_attempts_append_only()`,
	`CREATE INDEX IF NOT EXISTS decision_answer_attempts_task ON decision_answer_attempts(task_id, id)`,
	`CREATE TABLE IF NOT EXISTS decision_resolutions (
		resolution_id BIGSERIAL PRIMARY KEY,
		request_id TEXT NOT NULL,
		question_revision INT NOT NULL,
		revision_request_id TEXT NOT NULL,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		kind TEXT NOT NULL CHECK(kind IN ('answered','default_applied','withdrawn')),
		option_id TEXT,
		text TEXT,
		receipt TEXT,
		provenance TEXT NOT NULL CHECK(provenance IN ('operator_direct','operator_relay','assistant_judgement')),
		channel TEXT NOT NULL,
		decided_by TEXT NOT NULL,
		recorded_by TEXT NOT NULL,
		attempt_id BIGINT NOT NULL REFERENCES decision_answer_attempts(id),
		committed_at TIMESTAMPTZ NOT NULL,
		UNIQUE(request_id, question_revision)
	)`,
	`INSERT INTO schema_version(version) VALUES (41)`,
}

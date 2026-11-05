package store

// Version 38 adds the Discord outbox for decision requests
// (docs/decision-discord.md). A decision_notify row is the channel message of
// one decision_requests row: the transaction that opens the request inserts
// it, and the worker posts it, then edits it whenever the request's status
// moves past last_status (decision_notify.go). decision_notify_digest holds
// one row per UTC day, so one digest goes out per day however many servers
// run the worker.
//
// Both are new tables with their own indexes. No foreign key is created, so
// the version locks no existing table, and it writes no row: requests opened
// before the upgrade get no message of their own (the digest lists them once
// they are a day old).
var decisionNotifyV38 = []string{
	`CREATE TABLE IF NOT EXISTS decision_notify (
		request_id BIGINT PRIMARY KEY,
		message_id TEXT NOT NULL DEFAULT '',
		posted_at TIMESTAMPTZ,
		last_status TEXT NOT NULL DEFAULT '' CHECK(last_status IN ('','open','answered','superseded','expired','unsent','failed')),
		edited_at TIMESTAMPTZ,
		attempts INT NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS decision_notify_due ON decision_notify(next_attempt_at, request_id) WHERE last_status IN ('','open','expired')`,
	`CREATE TABLE IF NOT EXISTS decision_notify_digest (
		day DATE PRIMARY KEY,
		state TEXT NOT NULL CHECK(state IN ('pending','sent','empty','unsent','failed')),
		listed INT NOT NULL DEFAULT 0,
		message_id TEXT NOT NULL DEFAULT '',
		attempts INT NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		sent_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`INSERT INTO schema_version(version) VALUES (38)`,
}

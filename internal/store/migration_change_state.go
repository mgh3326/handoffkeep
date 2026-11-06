package store

// Version 50 makes hk the store for a pull request's live state (hk 3176,
// hk 3198; hk:doc research/2026-11-06/hk3176-pr-state-contract §0-§3 and §6).
// mergewake writes what its webhooks and GraphQL reads carry; readers ask hk.
//
// changes gains nullable columns, one block per section (state, checks,
// threads, findings, gate), each with the section's observed_at and source:
// the per-section freshness the write routes compare against. A NULL means
// the section was never written. Three tables hold the rows that do not fit a
// scalar: change_checks (one row per change, head and check name),
// change_threads (one row per GitHub thread node id) and change_findings
// (append-only per review pass, keyed by the pass and an ordinal, linked to
// the reviews row; a finding has no status column, its open or resolved
// state is derived from its thread at read time).
//
// Locks: the ALTER TABLE takes ACCESS EXCLUSIVE on changes for the length of
// the statement. Every added column is nullable with no default, so it
// rewrites nothing, and the CHECK constraints are validated against a table
// whose new columns are all NULL. The three CREATE TABLEs take SHARE ROW
// EXCLUSIVE on changes (and on reviews, for change_findings) through their
// foreign keys until the transaction commits, so change writers wait for it;
// the new tables are empty.
var changeStateV50 = []string{
	`ALTER TABLE changes
		ADD COLUMN IF NOT EXISTS draft BOOLEAN,
		ADD COLUMN IF NOT EXISTS base_ref TEXT,
		ADD COLUMN IF NOT EXISTS title TEXT,
		ADD COLUMN IF NOT EXISTS author TEXT,
		ADD COLUMN IF NOT EXISTS state_observed_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS state_source TEXT CHECK(state_source IN ('webhook','poll','fresh','review')),
		ADD COLUMN IF NOT EXISTS state_delivery_id TEXT,
		ADD COLUMN IF NOT EXISTS checks_observed_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS checks_source TEXT CHECK(checks_source IN ('webhook','poll','fresh','review')),
		ADD COLUMN IF NOT EXISTS threads_observed_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS threads_source TEXT CHECK(threads_source IN ('webhook','poll','fresh','review')),
		ADD COLUMN IF NOT EXISTS findings_observed_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS findings_source TEXT CHECK(findings_source IN ('webhook','poll','fresh','review')),
		ADD COLUMN IF NOT EXISTS gate_state TEXT CHECK(gate_state IN ('ready','blocked','pending')),
		ADD COLUMN IF NOT EXISTS gate_head TEXT CHECK(gate_head ~ '^[0-9a-f]{40}$'),
		ADD COLUMN IF NOT EXISTS gate_reasons JSONB,
		ADD COLUMN IF NOT EXISTS gate_decided_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS gate_policy_revision TEXT,
		ADD COLUMN IF NOT EXISTS gate_source TEXT CHECK(gate_source IN ('webhook','poll','fresh','review'))`,
	`CREATE TABLE IF NOT EXISTS change_checks (
		change_id BIGINT NOT NULL REFERENCES changes(id),
		head_sha TEXT NOT NULL CHECK(head_sha ~ '^[0-9a-f]{40}$'),
		name TEXT NOT NULL CHECK(name <> ''),
		state TEXT NOT NULL,
		conclusion TEXT,
		url TEXT,
		required BOOLEAN,
		started_at TIMESTAMPTZ,
		completed_at TIMESTAMPTZ,
		observed_at TIMESTAMPTZ NOT NULL,
		source TEXT NOT NULL CHECK(source IN ('webhook','poll','fresh','review')),
		gone BOOLEAN NOT NULL DEFAULT false,
		PRIMARY KEY(change_id, head_sha, name)
	)`,
	`CREATE TABLE IF NOT EXISTS change_threads (
		change_id BIGINT NOT NULL REFERENCES changes(id),
		thread_id TEXT NOT NULL CHECK(thread_id <> ''),
		url TEXT,
		path TEXT,
		line INT,
		author TEXT,
		author_kind TEXT NOT NULL CHECK(author_kind IN ('mergewake','bot','human')),
		severity TEXT CHECK(severity IN ('critical','high','medium','low')),
		resolved BOOLEAN NOT NULL,
		outdated BOOLEAN NOT NULL DEFAULT false,
		replied BOOLEAN NOT NULL DEFAULT false,
		comment_count INT CHECK(comment_count >= 0),
		last_comment_at TIMESTAMPTZ,
		excerpt TEXT,
		redacted BOOLEAN NOT NULL DEFAULT false,
		observed_at TIMESTAMPTZ NOT NULL,
		source TEXT NOT NULL CHECK(source IN ('webhook','poll','fresh','review')),
		gone BOOLEAN NOT NULL DEFAULT false,
		PRIMARY KEY(change_id, thread_id)
	)`,
	`CREATE TABLE IF NOT EXISTS change_findings (
		change_id BIGINT NOT NULL REFERENCES changes(id),
		reviewed_head TEXT NOT NULL,
		round INT NOT NULL,
		pass INT NOT NULL,
		lens TEXT NOT NULL,
		ordinal INT NOT NULL CHECK(ordinal >= 0),
		review_id BIGINT NOT NULL REFERENCES reviews(id),
		ext_id TEXT,
		severity TEXT NOT NULL CHECK(severity IN ('critical','high','medium','low')),
		path TEXT,
		line INT,
		title TEXT,
		body TEXT,
		thread_id TEXT,
		redacted BOOLEAN NOT NULL DEFAULT false,
		observed_at TIMESTAMPTZ NOT NULL,
		source TEXT NOT NULL CHECK(source IN ('webhook','poll','fresh','review')),
		PRIMARY KEY(change_id, reviewed_head, round, pass, lens, ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS change_findings_review ON change_findings(review_id)`,
	`INSERT INTO schema_version(version) VALUES (50)`,
}

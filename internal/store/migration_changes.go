package store

// Version 41 adds the work record's change tables (hk 3037; design
// hk:doc design/2026-11-05/hk-work-record-design-v1.1 §2 and §13.7). A change
// is one pull request, identified by (repo, pr_number) and shared by every
// task that names it; change_tasks links the two; reviews holds one row per
// review pass mergewake reports, with severity counts only (§0-1), never the
// findings. A NULL count means unknown, never zero. changes.priority,
// hold_reason and depends_on_change_id/depends_on_state are the merge-flow
// fields an operator sets (SetChangeMergeFlow); hk gives them, mergewake
// orders.
//
// All three are new tables with their own indexes and writes happen from the
// mergewake task events (recordChangeTx). The only existing table the
// migration touches is tasks: the change_tasks foreign key takes a short
// SHARE ROW EXCLUSIVE lock on it while the constraint is added, and the new
// table is empty, so the validation scan is instant.
var changesV41 = []string{
	`CREATE TABLE IF NOT EXISTS changes (
		id BIGSERIAL PRIMARY KEY,
		repo TEXT NOT NULL,
		pr_number INT NOT NULL CHECK(pr_number > 0),
		state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','merged','closed')),
		head_sha TEXT CHECK(head_sha ~ '^[0-9a-f]{40}$'),
		base_sha TEXT CHECK(base_sha ~ '^[0-9a-f]{40}$'),
		merge_sha TEXT CHECK(merge_sha ~ '^[0-9a-f]{40}$'),
		opened_at TIMESTAMPTZ NOT NULL,
		merged_at TIMESTAMPTZ,
		reverted_by_change_id BIGINT REFERENCES changes(id),
		priority INT NOT NULL DEFAULT 0 CHECK(priority >= 0),
		hold_reason TEXT,
		depends_on_change_id BIGINT REFERENCES changes(id),
		depends_on_state TEXT CHECK(depends_on_state IN ('merged','deployed')),
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		UNIQUE(repo, pr_number),
		CHECK((depends_on_change_id IS NULL) = (depends_on_state IS NULL)),
		CHECK(depends_on_change_id <> id)
	)`,
	`CREATE TABLE IF NOT EXISTS change_tasks (
		change_id BIGINT NOT NULL REFERENCES changes(id),
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		relation TEXT NOT NULL CHECK(relation IN ('implements','references')),
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY(change_id, task_id)
	)`,
	`CREATE INDEX IF NOT EXISTS change_tasks_task ON change_tasks(task_id)`,
	`CREATE TABLE IF NOT EXISTS reviews (
		id BIGSERIAL PRIMARY KEY,
		change_id BIGINT NOT NULL REFERENCES changes(id),
		reviewed_head TEXT NOT NULL,
		reviewed_base TEXT,
		patch_id TEXT,
		round INT NOT NULL,
		pass INT NOT NULL,
		lens TEXT NOT NULL,
		reviewer_job_id TEXT,
		family TEXT,
		profile TEXT,
		model TEXT,
		harness TEXT,
		effort TEXT,
		executor TEXT,
		fallback BOOLEAN,
		policy_revision TEXT,
		contract_source TEXT,
		verdict TEXT,
		critical INT CHECK(critical >= 0),
		high INT CHECK(high >= 0),
		medium INT CHECK(medium >= 0),
		low INT CHECK(low >= 0),
		started_at TIMESTAMPTZ,
		finished_at TIMESTAMPTZ,
		recorded_at TIMESTAMPTZ NOT NULL,
		UNIQUE(change_id, reviewed_head, lens, round, pass)
	)`,
	`INSERT INTO schema_version(version) VALUES (41)`,
}

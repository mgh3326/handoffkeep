package store

// Version 49 adds dots_results: one row per recorded dots comment, linking the
// document to the task whose Linear origin is the issue, the lane that was
// notified, the comment's own hash and whether the answer was adopted
// (design v1 §12.3, §13.1 R13). It is a new table, so it locks no existing
// row; its foreign keys take SHARE ROW EXCLUSIVE on tasks while they are
// created, which waits for task writers to commit and blocks them only for
// the length of this short transaction.
var dotsResultsV49 = []string{
	`CREATE TABLE IF NOT EXISTS dots_results (
		id BIGSERIAL PRIMARY KEY,
		document_key TEXT NOT NULL UNIQUE,
		comment_id TEXT NOT NULL UNIQUE,
		issue_uuid TEXT NOT NULL,
		issue_identifier TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('RESULT','BLOCKED','QUESTION')),
		verdict TEXT,
		artefact_sha256 TEXT NOT NULL CHECK(artefact_sha256 ~ '^[0-9a-f]{64}$'),
		task_id BIGINT REFERENCES tasks(id),
		routed_lane TEXT NOT NULL,
		adoption_state TEXT NOT NULL DEFAULT 'pending_review' CHECK(adoption_state IN ('pending_review','accepted','partially_used','rejected','superseded')),
		adopted_ref TEXT,
		adopted_by TEXT,
		adopted_at TIMESTAMPTZ,
		superseded_by BIGINT REFERENCES dots_results(id),
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		CHECK((adoption_state = 'superseded') = (superseded_by IS NOT NULL))
	)`,
	`CREATE INDEX IF NOT EXISTS dots_results_task ON dots_results(task_id) WHERE task_id IS NOT NULL`,
	`INSERT INTO schema_version(version) VALUES (49)`,
}

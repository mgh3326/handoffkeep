package store

// Version 41 records where a task came from by a durable external identity
// and lets a document name its task (hk 3038; design
// hk:doc design/2026-11-05/hk-work-record-design-v1.1 §2, §13.1 R14).
//
//   - task_origins keys an origin by (provider, namespace, external_id, task).
//     external_id is the provider's durable id (a Linear issue UUID); the
//     display number a person reads (MGH-151, later ROB-1514) is metadata in
//     display_ref and may change. A task may carry any number of origins, and
//     one external id may be the origin of several tasks. Origins between hk
//     tasks stay task_relations.
//   - documents.kind accepts decision, research, design, policy and evidence.
//     The old inline CHECK is dropped by looking its name up in pg_constraint
//     (an inline column CHECK is named documents_kind_check by default, but
//     the name is read, not guessed) and added again under that name.
//   - documents.task_id names the task a document belongs to; documents.
//     origin_class says how the document came to be (authored, job, import).
//     Both are nullable and NULL means unknown (design §1 rule 6).
//   - One backfill: the dots results written by the Linear webhook are
//     imports, a fact their key and created_by already state.
//
// Locks: CREATE TABLE task_origins takes SHARE ROW EXCLUSIVE on tasks for its
// foreign key, and so does ADD COLUMN task_id ... REFERENCES tasks(id) on
// documents; neither blocks readers. The documents changes (drop and re-add
// the CHECK, two ADD COLUMNs, the backfill UPDATE) take ACCESS EXCLUSIVE on
// documents for one scan: the new CHECK validates every row once, and the
// columns are added without a default, so no rewrite happens. CREATE INDEX
// takes SHARE on documents and on task_origins (empty) while it builds. The
// backfill UPDATE journals each changed document in sync_log, so sync clients
// pull them; the sync allocator takes its advisory lock at commit, after this
// migration's own lock (lockorder.go level 6).
var originsV41 = []string{
	`CREATE TABLE IF NOT EXISTS task_origins (
		id BIGSERIAL PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		provider TEXT NOT NULL CHECK(provider IN ('linear','github','sentry')),
		namespace TEXT NOT NULL,
		external_id TEXT NOT NULL,
		display_ref TEXT,
		url TEXT,
		title_snapshot TEXT,
		snapshot_at TIMESTAMPTZ NOT NULL,
		created_by TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE(provider, namespace, external_id, task_id)
	)`,
	`CREATE INDEX IF NOT EXISTS task_origins_external ON task_origins(provider, external_id)`,
	`DO $$ DECLARE c text; BEGIN
		FOR c IN SELECT conname FROM pg_constraint WHERE conrelid = 'documents'::regclass AND contype = 'c' AND conkey = ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid = 'documents'::regclass AND attname = 'kind')] LOOP
			EXECUTE format('ALTER TABLE documents DROP CONSTRAINT %I', c);
		END LOOP;
	END $$`,
	`ALTER TABLE documents
		ADD CONSTRAINT documents_kind_check CHECK(kind IN ('brief','report','answer','handoff','note','other','decision','research','design','policy','evidence')),
		ADD COLUMN IF NOT EXISTS task_id BIGINT REFERENCES tasks(id),
		ADD COLUMN IF NOT EXISTS origin_class TEXT CHECK(origin_class IN ('authored','job','import'))`,
	`CREATE INDEX IF NOT EXISTS documents_task ON documents(task_id) WHERE task_id IS NOT NULL`,
	`UPDATE documents SET origin_class = 'import' WHERE key LIKE 'dots/results/%' AND created_by = 'linear-webhook' AND origin_class IS NULL`,
	`INSERT INTO schema_version(version) VALUES (41)`,
}

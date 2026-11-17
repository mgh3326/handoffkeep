package store

// The tasks area on MySQL (hk 5074, B2). The B1b stub made `tasks` an id-only
// FK target; this migration gives it its columns, so every column and
// constraint step is guarded by the catalog. DDL commits implicitly, so each
// step is restartable.
//
// Locks: CREATE TABLE only locks the new table. The ALTERs of `tasks` and the
// index builds take a metadata lock on `tasks` while they run; MySQL has no
// users of this schema yet, so no blue-green writer waits on them.
//
// Decisions recorded here, each with the PostgreSQL object it replaces:
//   - Partial unique indexes become functional unique keys whose parts are NULL
//     when the predicate is false; MySQL admits any number of NULL keys, which
//     is the partial index's behaviour. The key names equal PostgreSQL's, which
//     is how the drift check pairs them.
//   - Foreign keys on task_events and task_relations are RESTRICT, not CASCADE:
//     MySQL fires no triggers for cascade actions, so a cascade could bypass the
//     append-only guard (design §11.2). Nothing deletes tasks.
//   - Free-form text is LONGTEXT, not TEXT: a task title may be up to ytwrite.TextMaxChars
//     (20M characters), far past TEXT's 64 KB.
//   - Text columns that sit in a key or index are bounded VARCHARs. Each limit is
//     the length the store validates, or a bound that fits InnoDB's 3,072-byte key
//     limit at 4 bytes per character.
//   - TIMESTAMPTZ is DATETIME(6) in UTC, JSONB is JSON, TEXT[] is a JSON array,
//     `from`/`to`/`by` stay column names and are quoted wherever they are used.
//   - checkpoints and memory carry no FULLTEXT index: nothing reads them (the API
//     is gone) and the design's ngram index belongs with the search work (B8).
const (
	mysqlTaskStateCheck = `state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged','deployed','dropped','duplicate','icebox','completed')`
	mysqlTaskKindCheck  = `kind IN ('implement','verify','research','fix','decide','ops') OR (kind='' AND state IN ('triage','dropped','duplicate'))`
	mysqlEventKinds     = `'transition','relane','decision','project','area','state_migration','awaiting_decision','parent','relation','auto_close','metadata','source_event'`
	mysqlTableOptions   = ` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`
)

// mysqlBulkGuard wraps a trigger body so the copy tool's bulk load (design §11.6)
// bypasses it. Only the copy tool's connection sets @hk_bulk_load.
func mysqlBulkGuard(body string) string {
	return `BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN ` + body + ` END IF; END`
}

func mysqlSyncEmit(name, table, event, model, row, action string) string {
	return `CREATE TRIGGER IF NOT EXISTS ` + name + ` AFTER ` + event + ` ON ` + table + ` FOR EACH ROW ` +
		mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'`+model+`',CAST(`+row+`.id AS CHAR),'`+action+`',UTC_TIMESTAMP(6));`)
}

var mysqlTasksSteps = []string{
	`CREATE TABLE IF NOT EXISTS task_projects (name VARCHAR(128) NOT NULL PRIMARY KEY, created_by LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, status LONGTEXT NOT NULL DEFAULT (''), start_at DATETIME(6), target_at DATETIME(6), design_lane LONGTEXT NOT NULL DEFAULT (''), CONSTRAINT task_projects_name_check CHECK(name REGEXP '^[A-Za-z0-9._-]{1,128}$'))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS project_milestones (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, project VARCHAR(128) NOT NULL, name LONGTEXT NOT NULL, description LONGTEXT NOT NULL DEFAULT (''), target_at DATETIME(6), status LONGTEXT NOT NULL DEFAULT (''), sort_order INT NOT NULL DEFAULT 0, CONSTRAINT project_milestones_project_fk FOREIGN KEY(project) REFERENCES task_projects(name) ON DELETE RESTRICT)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS project_owner_events (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, project VARCHAR(128) NOT NULL, previous_lane LONGTEXT NOT NULL DEFAULT (''), design_lane LONGTEXT NOT NULL DEFAULT (''), actor LONGTEXT NOT NULL, note LONGTEXT NOT NULL DEFAULT (''), created_at DATETIME(6) NOT NULL, KEY project_owner_events_project_id(project, id DESC), CONSTRAINT project_owner_events_project_fk FOREIGN KEY(project) REFERENCES task_projects(name) ON DELETE RESTRICT)` + mysqlTableOptions,

	// tasks: the stub from 20261117-mysql-comments has only id.
	mysqlIfNoColumn("tasks", "lane", `ALTER TABLE tasks
		ADD COLUMN lane VARCHAR(255) NOT NULL,
		ADD COLUMN parent_lane VARCHAR(255) NOT NULL DEFAULT '',
		ADD COLUMN title LONGTEXT NOT NULL,
		ADD COLUMN kind VARCHAR(16) NOT NULL,
		ADD COLUMN state VARCHAR(16) NOT NULL,
		ADD COLUMN priority INT NOT NULL DEFAULT 0,
		ADD COLUMN refs JSON NOT NULL DEFAULT (JSON_OBJECT()),
		ADD COLUMN claimed_by LONGTEXT NOT NULL DEFAULT (''),
		ADD COLUMN created_by LONGTEXT NOT NULL,
		ADD COLUMN created_at DATETIME(6) NOT NULL,
		ADD COLUMN updated_at DATETIME(6) NOT NULL,
		ADD COLUMN body_doc LONGTEXT NOT NULL DEFAULT (''),
		ADD COLUMN project VARCHAR(128),
		ADD COLUMN area LONGTEXT,
		ADD COLUMN expected LONGTEXT,
		ADD COLUMN source VARCHAR(128) NOT NULL DEFAULT '',
		ADD COLUMN source_ref VARCHAR(512) NOT NULL DEFAULT '',
		ADD COLUMN last_activity_at DATETIME(6) NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
		ADD COLUMN awaiting_decision BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN parent_id BIGINT,
		ADD COLUMN started_at DATETIME(6),
		ADD COLUMN completed_at DATETIME(6),
		ADD COLUMN canceled_at DATETIME(6),
		ADD COLUMN triaged_at DATETIME(6),
		ADD COLUMN started_triage_at DATETIME(6),
		ADD COLUMN milestone_id BIGINT,
		ADD COLUMN due_at DATETIME(6),
		ADD COLUMN sla_started_at DATETIME(6),
		ADD COLUMN sla_medium_risk_at DATETIME(6),
		ADD COLUMN sla_high_risk_at DATETIME(6),
		ADD COLUMN sla_breaches_at DATETIME(6)`),
	mysqlIfNoConstraint("tasks", "tasks_state_check", `ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK(`+mysqlTaskStateCheck+`)`),
	mysqlIfNoConstraint("tasks", "tasks_kind_check", `ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK(`+mysqlTaskKindCheck+`)`),
	mysqlIfNoConstraint("tasks", "tasks_closure_check", `ALTER TABLE tasks ADD CONSTRAINT tasks_closure_check CHECK(JSON_EXTRACT(refs,'$.closure') IS NULL OR (JSON_UNQUOTE(JSON_EXTRACT(refs,'$.closure.type')) IN ('resolved','cancelled','obsolete','superseded') AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(refs,'$.closure.evidence')),'') <> ''))`),
	mysqlIfNoConstraint("tasks", "tasks_parent_fk", `ALTER TABLE tasks ADD CONSTRAINT tasks_parent_fk FOREIGN KEY(parent_id) REFERENCES tasks(id) ON DELETE RESTRICT`),
	mysqlIfNoConstraint("tasks", "tasks_milestone_fk", `ALTER TABLE tasks ADD CONSTRAINT tasks_milestone_fk FOREIGN KEY(milestone_id) REFERENCES project_milestones(id) ON DELETE RESTRICT`),
	mysqlIfNoIndex("tasks", "tasks_lane_state_priority", `CREATE INDEX tasks_lane_state_priority ON tasks(lane, state, priority DESC, created_at ASC, id ASC)`),
	mysqlIfNoIndex("tasks", "tasks_parent_lane_state_priority", `CREATE INDEX tasks_parent_lane_state_priority ON tasks(parent_lane, state, priority DESC, created_at ASC, id ASC)`),
	mysqlIfNoIndex("tasks", "tasks_parent", `CREATE INDEX tasks_parent ON tasks(parent_id, id)`),
	// PostgreSQL: UNIQUE (source, source_ref) WHERE source<>'' AND source_ref<>''.
	mysqlIfNoIndex("tasks", "tasks_source_ref", `CREATE UNIQUE INDEX tasks_source_ref ON tasks((CASE WHEN source<>'' AND source_ref<>'' THEN source END),(CASE WHEN source<>'' AND source_ref<>'' THEN source_ref END))`),

	`CREATE TABLE IF NOT EXISTS task_events (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, task_id BIGINT NOT NULL, ` + "`from`" + ` LONGTEXT NOT NULL, ` + "`to`" + ` LONGTEXT NOT NULL, ` + "`by`" + ` LONGTEXT NOT NULL, note LONGTEXT NOT NULL DEFAULT (''), at DATETIME(6) NOT NULL, refs JSON, kind VARCHAR(32) NOT NULL DEFAULT 'transition', no_job LONGTEXT NOT NULL DEFAULT (''), source VARCHAR(128) NOT NULL DEFAULT '', source_ref VARCHAR(512) NOT NULL DEFAULT '', field LONGTEXT NOT NULL DEFAULT (''), from_value LONGTEXT NOT NULL DEFAULT (''), to_value LONGTEXT NOT NULL DEFAULT (''), auto BOOLEAN NOT NULL DEFAULT FALSE, applied BOOLEAN NOT NULL DEFAULT TRUE, ` +
		`CONSTRAINT task_events_kind_check CHECK(kind IN (` + mysqlEventKinds + `)), ` +
		`KEY task_events_task_at(task_id, at ASC, id ASC), ` +
		// PostgreSQL: UNIQUE (task_id, source, source_ref) WHERE source<>'' AND source_ref<>''.
		`UNIQUE KEY task_events_source_ref(task_id, (CASE WHEN source<>'' AND source_ref<>'' THEN source END), (CASE WHEN source<>'' AND source_ref<>'' THEN source_ref END)), ` +
		`CONSTRAINT task_events_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,
	`CREATE TRIGGER IF NOT EXISTS task_events_guard_update BEFORE UPDATE ON task_events FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='task_events is append-only'; END IF; END`,
	`CREATE TRIGGER IF NOT EXISTS task_events_guard_delete BEFORE DELETE ON task_events FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='task_events is append-only'; END IF; END`,

	`CREATE TABLE IF NOT EXISTS task_relations (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, kind VARCHAR(16) NOT NULL, src_id BIGINT NOT NULL, dst_id BIGINT NOT NULL, created_by LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, archived_at DATETIME(6), archived_by LONGTEXT NOT NULL DEFAULT (''), ` +
		`CONSTRAINT task_relations_kind_check CHECK(kind IN ('blocks','duplicate_of','related')), CONSTRAINT task_relations_ends_check CHECK(src_id<>dst_id), CONSTRAINT task_relations_related_check CHECK(kind<>'related' OR src_id<dst_id), ` +
		`UNIQUE KEY task_relations_identity(kind, src_id, dst_id), KEY task_relations_dst(dst_id, kind), ` +
		`CONSTRAINT task_relations_src_fk FOREIGN KEY(src_id) REFERENCES tasks(id) ON DELETE RESTRICT, CONSTRAINT task_relations_dst_fk FOREIGN KEY(dst_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS task_origins (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, task_id BIGINT NOT NULL, provider VARCHAR(16) NOT NULL, namespace VARCHAR(255) NOT NULL, external_id VARCHAR(255) NOT NULL, display_ref LONGTEXT, url LONGTEXT, title_snapshot LONGTEXT, snapshot_at DATETIME(6) NOT NULL, created_by LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, tenant LONGTEXT NOT NULL DEFAULT (''), role VARCHAR(16) NOT NULL DEFAULT 'origin', synced_at DATETIME(6), ` +
		`CONSTRAINT task_origins_provider_check CHECK(provider IN ('linear','github','sentry','youtrack')), CONSTRAINT task_origins_role_check CHECK(role IN ('origin','mirror')), ` +
		`UNIQUE KEY task_origins_identity(provider, namespace, external_id, task_id), KEY task_origins_external(provider, external_id), ` +
		// PostgreSQL: UNIQUE (task_id) WHERE provider='youtrack', and UNIQUE (namespace, external_id) WHERE provider='youtrack'.
		`UNIQUE KEY task_origins_youtrack_task((CASE WHEN provider='youtrack' THEN task_id END)), ` +
		`UNIQUE KEY task_origins_youtrack_external((CASE WHEN provider='youtrack' THEN namespace END),(CASE WHEN provider='youtrack' THEN external_id END)), ` +
		`CONSTRAINT task_origins_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS ticket_sets (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, source VARCHAR(128) NOT NULL, source_ref VARCHAR(512) NOT NULL, payload_sha256 CHAR(64) NOT NULL, parent_task_id BIGINT NOT NULL, created_at DATETIME(6) NOT NULL, ` +
		`CONSTRAINT ticket_sets_source_check CHECK(source <> ''), CONSTRAINT ticket_sets_source_ref_check CHECK(source_ref <> ''), CONSTRAINT ticket_sets_sha_check CHECK(payload_sha256 REGEXP '^[0-9a-f]{64}$'), ` +
		`UNIQUE KEY ticket_sets_parent_task_id_key(parent_task_id), UNIQUE KEY ticket_sets_identity(source, source_ref), ` +
		`CONSTRAINT ticket_sets_parent_fk FOREIGN KEY(parent_task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS task_spawn_records (task_id BIGINT NOT NULL PRIMARY KEY, request_id LONGTEXT NOT NULL, job LONGTEXT NOT NULL, role LONGTEXT NOT NULL DEFAULT (''), state VARCHAR(16) NOT NULL, reason LONGTEXT NOT NULL DEFAULT (''), attempt INT NOT NULL, host LONGTEXT NOT NULL DEFAULT (''), next_check_at DATETIME(6), expires_at DATETIME(6), updated_at DATETIME(6) NOT NULL, start_task_id BIGINT, start_until VARCHAR(16), ` +
		`CONSTRAINT task_spawn_records_state_check CHECK(state IN ('waiting','pending','parked','bound','launching','reconciling','landed','refused','given_up','cancelled')), CONSTRAINT task_spawn_records_attempt_check CHECK(attempt >= 1), CONSTRAINT task_spawn_records_start_until_check CHECK(start_until IN ('merged','deployed')), CONSTRAINT task_spawn_records_start_pair_check CHECK((start_task_id IS NULL) = (start_until IS NULL)), ` +
		`CONSTRAINT task_spawn_records_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS task_spawn_notices (notice_key VARCHAR(255) NOT NULL PRIMARY KEY, task_id BIGINT NOT NULL, created_at DATETIME(6) NOT NULL, CONSTRAINT task_spawn_notices_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS task_thread_deliveries (task_id BIGINT NOT NULL, destination VARCHAR(16) NOT NULL, thread_id VARCHAR(32), thread_name LONGTEXT NOT NULL, start_event_id BIGINT NOT NULL, since DATETIME(6) NOT NULL, last_event_id BIGINT NOT NULL, posted JSON NOT NULL DEFAULT (JSON_ARRAY()), state VARCHAR(16) NOT NULL, claim_key LONGTEXT NOT NULL DEFAULT (''), claimed_at DATETIME(6), next_attempt_at DATETIME(6) NOT NULL, attempts INT NOT NULL DEFAULT 0, last_error LONGTEXT NOT NULL DEFAULT (''), created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, ` +
		`PRIMARY KEY(task_id, destination), KEY task_thread_deliveries_due(destination, state, next_attempt_at), ` +
		`CONSTRAINT task_thread_deliveries_destination_check CHECK(destination IN ('discord')), CONSTRAINT task_thread_deliveries_thread_check CHECK(thread_id REGEXP '^[0-9]{1,32}$'), CONSTRAINT task_thread_deliveries_state_check CHECK(state IN ('pending','creating','active','needs_operator','failed')), ` +
		`CONSTRAINT task_thread_deliveries_active_check CHECK((state = 'active') = (thread_id IS NOT NULL) OR state IN ('needs_operator','failed')), CONSTRAINT task_thread_deliveries_claim_check CHECK((claim_key = '') = (claimed_at IS NULL)))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS task_thread_cursors (destination VARCHAR(16) NOT NULL PRIMARY KEY, epoch_event_id BIGINT NOT NULL, scan_event_id BIGINT NOT NULL, updated_at DATETIME(6) NOT NULL, CONSTRAINT task_thread_cursors_destination_check CHECK(destination IN ('discord')))` + mysqlTableOptions,

	// The decision projection the tasks trigger maintains, and its feed emit.
	`CREATE TABLE IF NOT EXISTS sync_decisions (id VARCHAR(512) NOT NULL PRIMARY KEY, task_id BIGINT NOT NULL, updated_at DATETIME(6) NOT NULL, UNIQUE KEY sync_decisions_task_id_key(task_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS checkpoints (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, session VARCHAR(255) NOT NULL, kind VARCHAR(16) NOT NULL, title LONGTEXT NOT NULL, body LONGTEXT NOT NULL, refs JSON NOT NULL DEFAULT (JSON_OBJECT()), created_by LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, ` +
		`CONSTRAINT checkpoints_kind_check CHECK(kind IN ('checkpoint','handoff','decision','open_question','next_action')), KEY checkpoints_session_created(session, created_at DESC, id DESC))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS memory (agent VARCHAR(255) NOT NULL, name VARCHAR(255) NOT NULL, description LONGTEXT NOT NULL, memory_type VARCHAR(16) NOT NULL, content LONGTEXT NOT NULL, updated_by LONGTEXT NOT NULL, updated_at DATETIME(6) NOT NULL, ` +
		`CONSTRAINT memory_type_check CHECK(memory_type IN ('user','feedback','project','reference')), UNIQUE KEY memory_identity(agent, name), KEY memory_agent_updated(agent, updated_at DESC, name DESC))` + mysqlTableOptions,
}

// The triggers come last: they name sync_log, sync_decisions and tasks columns.
// Each is guarded by @hk_bulk_load like the guards of 20261117-mysql-comments.
var mysqlTasksTriggers = []string{
	// PostgreSQL: CHECK(parent_id IS NULL OR parent_id<>id). MySQL refuses a CHECK
	// that names the AUTO_INCREMENT column (error 3818), so a task that is its own
	// parent is refused by triggers, with the check-violation error number.
	`CREATE TRIGGER IF NOT EXISTS tasks_parent_check_insert BEFORE INSERT ON tasks FOR EACH ROW BEGIN IF NEW.parent_id IS NOT NULL AND NEW.parent_id = NEW.id THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO = 3819, MESSAGE_TEXT = 'tasks_parent_check'; END IF; END`,
	`CREATE TRIGGER IF NOT EXISTS tasks_parent_check_update BEFORE UPDATE ON tasks FOR EACH ROW BEGIN IF NEW.parent_id IS NOT NULL AND NEW.parent_id = NEW.id THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO = 3819, MESSAGE_TEXT = 'tasks_parent_check'; END IF; END`,
	mysqlSyncEmit("sync_tasks_insert", "tasks", "INSERT", "task", "NEW", "I"),
	mysqlSyncEmit("sync_tasks_update", "tasks", "UPDATE", "task", "NEW", "U"),
	mysqlSyncEmit("sync_tasks_delete", "tasks", "DELETE", "task", "OLD", "D"),
	// sync_decisions rows are keyed by request id, not a numeric id.
	`CREATE TRIGGER IF NOT EXISTS sync_decisions_insert AFTER INSERT ON sync_decisions FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',NEW.id,'I',UTC_TIMESTAMP(6));`),
	`CREATE TRIGGER IF NOT EXISTS sync_decisions_update AFTER UPDATE ON sync_decisions FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',NEW.id,'U',UTC_TIMESTAMP(6));`),
	`CREATE TRIGGER IF NOT EXISTS sync_decisions_delete AFTER DELETE ON sync_decisions FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',OLD.id,'D',UTC_TIMESTAMP(6));`),
	// sync_task_decision: keep sync_decisions equal to the task's refs.decision_request.id.
	`CREATE TRIGGER IF NOT EXISTS sync_task_decision_delete AFTER DELETE ON tasks FOR EACH ROW ` + mysqlBulkGuard(`DELETE FROM sync_decisions WHERE task_id=OLD.id;`),
	mysqlTaskDecision("sync_task_decision_insert", "INSERT", false),
	mysqlTaskDecision("sync_task_decision_update", "UPDATE", true),
}

// mysqlTaskDecision keeps sync_decisions equal to the task's
// refs.decision_request.id, as PostgreSQL's handoffkeep_sync_decision does. An
// UPDATE that leaves the decision_request object unchanged does nothing.
// JSON null and non-string ids count as no id, as ->> gives SQL NULL there.
func mysqlTaskDecision(name, event string, update bool) string {
	guard := ``
	end := ``
	if update {
		guard = `IF NOT (JSON_EXTRACT(OLD.refs,'$.decision_request') <=> JSON_EXTRACT(NEW.refs,'$.decision_request')) THEN `
		end = ` END IF;`
	}
	return `CREATE TRIGGER IF NOT EXISTS ` + name + ` AFTER ` + event + ` ON tasks FOR EACH ROW BEGIN DECLARE request_id LONGTEXT; IF COALESCE(@hk_bulk_load,0) <> 1 THEN ` + guard +
		`SET request_id = IF(JSON_TYPE(JSON_EXTRACT(NEW.refs,'$.decision_request.id'))='STRING', JSON_UNQUOTE(JSON_EXTRACT(NEW.refs,'$.decision_request.id')), NULL); ` +
		`DELETE FROM sync_decisions WHERE task_id=NEW.id AND NOT (id <=> request_id); ` +
		`IF COALESCE(request_id,'') <> '' THEN INSERT INTO sync_decisions(id,task_id,updated_at) VALUES(request_id,NEW.id,NEW.updated_at) ON DUPLICATE KEY UPDATE task_id=VALUES(task_id), updated_at=VALUES(updated_at); END IF;` + end + ` END IF; END`
}

func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261118-mysql-tasks",
		after: []string{"20261117-mysql-comments", "20261117-mysql-sync", "20261117-mysql-locks", "20261117-mysql-bulk-guards"},
		steps: append(append([]string{}, mysqlTasksSteps...), mysqlTasksTriggers...),
	})
}

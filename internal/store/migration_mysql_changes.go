package store

// This is the baseline schema for the changes/deploy area (hk 5078, design
// hk:doc design/2026-11-17/hk4899-mysql §11). The tasks migration creates the
// task FK targets; this migration adds the change, review and deploy tables.
// MySQL commits DDL implicitly, so each step is independently restartable.
// Locks: each CREATE TABLE locks only its new table; CREATE TRIGGER briefly
// takes a metadata lock on changes. There are no MySQL production writers yet.
func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261117-mysql-changes",
		after: []string{"20261118-mysql-tasks"},
		steps: mysqlChangesSteps,
	})
}

var mysqlChangesSteps = []string{
	`CREATE TABLE IF NOT EXISTS changes (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		repo VARCHAR(766) NOT NULL,
		pr_number INT NOT NULL,
		state VARCHAR(16) NOT NULL DEFAULT 'open',
		head_sha VARCHAR(64), base_sha VARCHAR(64), merge_sha VARCHAR(64),
		opened_at DATETIME(6) NOT NULL, merged_at DATETIME(6),
		reverted_by_change_id BIGINT, priority INT NOT NULL DEFAULT 0,
		hold_reason LONGTEXT, depends_on_change_id BIGINT, depends_on_state VARCHAR(16),
		created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
		draft BOOLEAN, base_ref LONGTEXT, title LONGTEXT, author LONGTEXT,
		state_observed_at DATETIME(6), state_source VARCHAR(16), state_delivery_id VARCHAR(255),
		checks_observed_at DATETIME(6), checks_source VARCHAR(16),
		threads_observed_at DATETIME(6), threads_source VARCHAR(16),
		findings_observed_at DATETIME(6), findings_source VARCHAR(16),
		gate_state VARCHAR(16), gate_head VARCHAR(64), gate_reasons JSON,
		gate_decided_at DATETIME(6), gate_policy_revision LONGTEXT, gate_source VARCHAR(16),
		UNIQUE KEY changes_repo_pr(repo,pr_number),
		KEY changes_reverted_by(reverted_by_change_id), KEY changes_depends_on(depends_on_change_id),
		CONSTRAINT changes_state_check CHECK(state IN ('open','merged','closed')),
		CONSTRAINT changes_pr_check CHECK(pr_number>0), CONSTRAINT changes_priority_check CHECK(priority>=0),
		CONSTRAINT changes_depends_state_check CHECK(depends_on_state IS NULL OR depends_on_state IN ('merged','deployed')),
		CONSTRAINT changes_depends_pair_check CHECK((depends_on_change_id IS NULL)=(depends_on_state IS NULL)),
		CONSTRAINT changes_gate_state_check CHECK(gate_state IS NULL OR gate_state IN ('ready','blocked','pending')),
		CONSTRAINT changes_state_source_check CHECK(state_source IS NULL OR state_source IN ('webhook','poll','fresh','review')),
		CONSTRAINT changes_checks_source_check CHECK(checks_source IS NULL OR checks_source IN ('webhook','poll','fresh','review')),
		CONSTRAINT changes_threads_source_check CHECK(threads_source IS NULL OR threads_source IN ('webhook','poll','fresh','review')),
		CONSTRAINT changes_findings_source_check CHECK(findings_source IS NULL OR findings_source IN ('webhook','poll','fresh','review')),
		CONSTRAINT changes_gate_source_check CHECK(gate_source IS NULL OR gate_source IN ('webhook','poll','fresh','review')),
		CONSTRAINT changes_reverted_fk FOREIGN KEY(reverted_by_change_id) REFERENCES changes(id) ON DELETE RESTRICT,
		CONSTRAINT changes_depends_fk FOREIGN KEY(depends_on_change_id) REFERENCES changes(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS change_tasks (
		change_id BIGINT NOT NULL, task_id BIGINT NOT NULL, relation VARCHAR(16) NOT NULL, created_at DATETIME(6) NOT NULL,
		PRIMARY KEY(change_id,task_id), KEY change_tasks_task(task_id),
		CONSTRAINT change_tasks_relation_check CHECK(relation IN ('implements','references')),
		CONSTRAINT change_tasks_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT,
		CONSTRAINT change_tasks_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS reviews (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, change_id BIGINT NOT NULL,
		reviewed_head VARCHAR(64) NOT NULL, reviewed_base VARCHAR(64), patch_id LONGTEXT,
		round INT NOT NULL, pass INT NOT NULL, lens VARCHAR(64) NOT NULL, reviewer_job_id VARCHAR(255),
		family VARCHAR(64), profile LONGTEXT, model LONGTEXT, harness LONGTEXT, effort VARCHAR(32), executor LONGTEXT,
		fallback BOOLEAN, policy_revision LONGTEXT, contract_source LONGTEXT, verdict VARCHAR(32),
		critical INT, high INT, medium INT, low INT, started_at DATETIME(6), finished_at DATETIME(6), recorded_at DATETIME(6) NOT NULL,
		reviewer LONGTEXT, same_family BOOLEAN, delta BOOLEAN, quiet BOOLEAN, outside_diff_blocking INT, patch_digest VARCHAR(128),
		UNIQUE KEY reviews_pass(change_id,reviewed_head,lens,round,pass), KEY reviews_change_id(change_id),
		CONSTRAINT reviews_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT,
		CONSTRAINT reviews_counts_check CHECK((critical IS NULL OR critical>=0) AND (high IS NULL OR high>=0) AND (medium IS NULL OR medium>=0) AND (low IS NULL OR low>=0) AND (outside_diff_blocking IS NULL OR outside_diff_blocking>=0))
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS change_checks (
		change_id BIGINT NOT NULL, head_sha VARCHAR(64) NOT NULL, name VARCHAR(512) NOT NULL, state LONGTEXT NOT NULL,
		conclusion LONGTEXT, url LONGTEXT, required BOOLEAN, started_at DATETIME(6), completed_at DATETIME(6),
		observed_at DATETIME(6) NOT NULL, source VARCHAR(16) NOT NULL, gone BOOLEAN NOT NULL DEFAULT FALSE, delivery_id VARCHAR(255),
		PRIMARY KEY(change_id,head_sha,name),
		CONSTRAINT change_checks_source_check CHECK(source IN ('webhook','poll','fresh','review')),
		CONSTRAINT change_checks_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS change_threads (
		change_id BIGINT NOT NULL, thread_id VARCHAR(700) NOT NULL, url LONGTEXT, path LONGTEXT, line INT, author LONGTEXT,
		author_kind VARCHAR(16) NOT NULL, severity VARCHAR(16), resolved BOOLEAN NOT NULL, outdated BOOLEAN NOT NULL DEFAULT FALSE,
		replied BOOLEAN NOT NULL DEFAULT FALSE, comment_count INT, last_comment_at DATETIME(6), excerpt LONGTEXT,
		redacted BOOLEAN NOT NULL DEFAULT FALSE, observed_at DATETIME(6) NOT NULL, source VARCHAR(16) NOT NULL, gone BOOLEAN NOT NULL DEFAULT FALSE,
		PRIMARY KEY(change_id,thread_id),
		CONSTRAINT change_threads_source_check CHECK(source IN ('webhook','poll','fresh','review')),
		CONSTRAINT change_threads_kind_check CHECK(author_kind IN ('mergewake','bot','human')),
		CONSTRAINT change_threads_severity_check CHECK(severity IS NULL OR severity IN ('critical','high','medium','low')),
		CONSTRAINT change_threads_count_check CHECK(comment_count IS NULL OR comment_count>=0),
		CONSTRAINT change_threads_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS change_findings (
		change_id BIGINT NOT NULL, reviewed_head VARCHAR(64) NOT NULL, round INT NOT NULL, pass INT NOT NULL, lens VARCHAR(64) NOT NULL,
		ordinal INT NOT NULL, review_id BIGINT NOT NULL, ext_id VARCHAR(512) NOT NULL, location LONGTEXT, severity VARCHAR(16),
		blocking BOOLEAN NOT NULL DEFAULT FALSE, summary LONGTEXT, url LONGTEXT, author_replied BOOLEAN NOT NULL DEFAULT FALSE,
		thread_id VARCHAR(512), body LONGTEXT, redacted BOOLEAN NOT NULL DEFAULT FALSE, observed_at DATETIME(6) NOT NULL, source VARCHAR(16) NOT NULL,
		PRIMARY KEY(change_id,reviewed_head,round,pass,lens,ordinal), KEY change_findings_review(review_id),
		CONSTRAINT change_findings_source_check CHECK(source IN ('webhook','poll','fresh','review')),
		CONSTRAINT change_findings_severity_check CHECK(severity IS NULL OR severity IN ('critical','high','medium','low')),
		CONSTRAINT change_findings_ordinal_check CHECK(ordinal>=0),
		CONSTRAINT change_findings_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT,
		CONSTRAINT change_findings_review_fk FOREIGN KEY(review_id) REFERENCES reviews(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS change_gate_events (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, change_id BIGINT NOT NULL, head_sha VARCHAR(64) NOT NULL,
		state VARCHAR(16) NOT NULL, changed BOOLEAN NOT NULL, reasons JSON NOT NULL, policy_revision LONGTEXT,
		decided_at DATETIME(6) NOT NULL, source VARCHAR(16) NOT NULL, recorded_at DATETIME(6) NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
		UNIQUE KEY change_gate_events_decision(change_id,head_sha,state,decided_at),
		CONSTRAINT change_gate_events_state_check CHECK(state IN ('ready','blocked','pending')),
		CONSTRAINT change_gate_events_source_check CHECK(source IN ('webhook','poll','fresh','review')),
		CONSTRAINT change_gate_events_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS deploy_record_projection (` + "`key`" + ` VARCHAR(767) NOT NULL PRIMARY KEY, sha256 VARCHAR(64) NOT NULL, record JSON NOT NULL, decoded BOOLEAN NOT NULL
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS deploy_services (
		name VARCHAR(128) NOT NULL PRIMARY KEY, repo LONGTEXT NOT NULL, record_prefixes JSON NOT NULL,
		active BOOLEAN NOT NULL, created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`INSERT INTO deploy_services(name,repo,record_prefixes,active,created_at,updated_at) VALUES
		('handoffkeep','mgh3326/handoffkeep',JSON_ARRAY('deploy/handoffkeep/'),TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('auto_trader','mgh3326/auto_trader',JSON_ARRAY('deploy/auto_trader/'),TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('panewire-hub','mgh3326/panewire',JSON_ARRAY('deploy/panewire-hub/','deploy/panewire/'),TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('mergewake','mgh3326/mergewake',JSON_ARRAY('deploy/mergewake/'),TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('pacelever','mgh3326/pacelever',JSON_ARRAY('deploy/pacelever/'),TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))
		ON DUPLICATE KEY UPDATE name=name`,
	`CREATE TRIGGER IF NOT EXISTS change_gate_events_record AFTER UPDATE ON changes FOR EACH ROW
		BEGIN
			IF COALESCE(@hk_bulk_load,0) <> 1 AND NEW.gate_state IS NOT NULL AND NEW.gate_head IS NOT NULL AND NEW.gate_decided_at IS NOT NULL
			AND (NOT (OLD.gate_state <=> NEW.gate_state) OR NOT (OLD.gate_head <=> NEW.gate_head) OR NOT (OLD.gate_decided_at <=> NEW.gate_decided_at)) THEN
				INSERT INTO change_gate_events(change_id,head_sha,state,changed,reasons,policy_revision,decided_at,source)
				VALUES(NEW.id,NEW.gate_head,NEW.gate_state,NOT (OLD.gate_state <=> NEW.gate_state) OR NOT (OLD.gate_head <=> NEW.gate_head),COALESCE(NEW.gate_reasons,JSON_ARRAY()),NEW.gate_policy_revision,NEW.gate_decided_at,COALESCE(NEW.gate_source,'review'))
				ON DUPLICATE KEY UPDATE id=id;
			END IF;
		END`,
}

package store

// The baseline is additive and every step is restartable after MySQL's
// implicit DDL commits. All text uses the NO PAD binary collation.
//
// The quota history triggers are hoisted so the collation migration can
// rebuild them under the pinned connection collation (design §11.4, hk 5169).
var mysqlBaselineQuotaTriggers = []string{
	// MySQL has no statement-level TRUNCATE trigger. The application user must
	// lack DROP privilege, which is required for TRUNCATE.
	`CREATE TRIGGER IF NOT EXISTS quota_history_immutable_update BEFORE UPDATE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
	`CREATE TRIGGER IF NOT EXISTS quota_history_immutable_delete BEFORE DELETE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
}

// Retired by operator decision 501, hk 6389: new installs no longer create
// bench_scores or bench_area_exceptions; existing databases keep the empty
// tables until 20261127-retire-bench-tables (hk 6390) drops them.
var mysqlBaseline = mysqlMigration{id: "20261117-quota-bench-baseline", steps: append([]string{
	`CREATE TABLE IF NOT EXISTS quota_observations (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, account_key VARCHAR(64) NOT NULL, kind VARCHAR(16) NOT NULL CHECK (kind IN ('auto','manual')), window_key VARCHAR(16) NOT NULL, measured_at DATETIME(6) NOT NULL, value JSON NOT NULL, received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS quota_latest (account_key VARCHAR(64) NOT NULL, kind VARCHAR(16) NOT NULL, window_key VARCHAR(16) NOT NULL, observation_id BIGINT NOT NULL, measured_at DATETIME(6) NOT NULL, confirmed_at DATETIME(6), PRIMARY KEY(account_key,kind,window_key), FOREIGN KEY(observation_id) REFERENCES quota_observations(id)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS quota_leases (account_key VARCHAR(64) NOT NULL PRIMARY KEY, owner TEXT NOT NULL, holder TEXT NOT NULL, expires_at DATETIME(6) NOT NULL) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS quota_presence (host_id VARCHAR(64) NOT NULL, account_key VARCHAR(64) NOT NULL, last_seen DATETIME(6) NOT NULL, PRIMARY KEY(host_id,account_key), KEY quota_presence_account(account_key), CHECK(CHAR_LENGTH(host_id) BETWEEN 1 AND 64), CHECK(CHAR_LENGTH(account_key) BETWEEN 1 AND 64)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS bench_reps (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, origin_id BIGINT NOT NULL CHECK(origin_id >= 1), profile TEXT NOT NULL, model_id TEXT, task_ref TEXT, tier TEXT, role TEXT, rounds INTEGER, blockers_found INTEGER, completed INTEGER, input_tokens BIGINT, output_tokens BIGINT, notes TEXT, recorded_at DATETIME(6) NOT NULL, effort TEXT, grade TEXT, table_grade TEXT, created_by VARCHAR(128) NOT NULL, created_at DATETIME(6) NOT NULL, experiment_id TEXT, arm TEXT CHECK(arm IN ('baseline','arm')), first_ci_green BOOLEAN, critical_findings INT CHECK(critical_findings >= 0), high_findings INT CHECK(high_findings >= 0), merged BOOLEAN, reverted_within_7d BOOLEAN, reviewer_profile TEXT, reviewer_effort TEXT, reasoning_tokens BIGINT CHECK(reasoning_tokens >= 0), cache_read_tokens BIGINT CHECK(cache_read_tokens >= 0), cache_write_tokens BIGINT CHECK(cache_write_tokens >= 0), spawn_to_ready_s INT CHECK(spawn_to_ready_s >= 0), spawn_to_merge_s INT CHECK(spawn_to_merge_s >= 0), UNIQUE KEY bench_reps_identity(created_by,origin_id)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS bench_grades (profile VARCHAR(200) NOT NULL PRIMARY KEY, grade VARCHAR(16) NOT NULL CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')), boundary_version TEXT NOT NULL DEFAULT (''), deviation_ref TEXT NOT NULL, decided_at DATETIME(6) NOT NULL, decided_by TEXT NOT NULL) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	`CREATE TABLE IF NOT EXISTS bench_catalog (profile VARCHAR(200) NOT NULL, effort VARCHAR(200) NOT NULL DEFAULT '', model_id TEXT NOT NULL, pool TEXT NOT NULL, grade VARCHAR(16) NOT NULL CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')), score DOUBLE, gate VARCHAR(32) NOT NULL DEFAULT 'default' CHECK(gate IN ('default','escalation','consult_only')), gate_reason TEXT, benchmark_source TEXT, benchmark_annotation TEXT, boundary_version TEXT NOT NULL DEFAULT (''), deviation_ref TEXT NOT NULL, decided_at DATETIME(6) NOT NULL, decided_by TEXT NOT NULL, retired_at DATETIME(6), PRIMARY KEY(profile,effort)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
}, mysqlBaselineQuotaTriggers...)}

func init() { registerMySQLMigration(mysqlBaseline) }

package store

// The verification ledger on MySQL (hk 5914), the port of migration_verifications.go
// (PostgreSQL version 49). It is a new table, so it is expand-only: nothing
// the previous release reads changes. CREATE TABLE locks only this table; the
// foreign key takes a metadata lock on changes briefly, as the changes
// migration's own tables do. evidence is LONGTEXT because an hk:doc key may be
// up to 512 bytes plus its "hk:doc " prefix, more than VARCHAR(512) holds.
var mysqlVerificationsSteps = []string{
	`CREATE TABLE IF NOT EXISTS verifications (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		change_id BIGINT NOT NULL,
		head_sha VARCHAR(64) NOT NULL CHECK(head_sha REGEXP '^[0-9a-f]{40}$'),
		level VARCHAR(32) NOT NULL,
		verifier VARCHAR(128) NOT NULL,
		verifier_job_id VARCHAR(128),
		family VARCHAR(128) NOT NULL,
		profile VARCHAR(128), model VARCHAR(128), harness VARCHAR(128),
		evidence LONGTEXT NOT NULL,
		ci_result VARCHAR(16),
		source VARCHAR(16) NOT NULL,
		recorded_by VARCHAR(128) NOT NULL,
		recorded_at DATETIME(6) NOT NULL,
		UNIQUE KEY verifications_change_head(change_id,head_sha),
		CONSTRAINT verifications_level_check CHECK(level IN ('live-verified','unit-test-verified','type-check-only','verifier-blocked','verifier-failed')),
		CONSTRAINT verifications_ci_check CHECK(ci_result IS NULL OR ci_result IN ('success','failure','pending','none')),
		CONSTRAINT verifications_source_check CHECK(source IN ('direct_merge','mergewake')),
		CONSTRAINT verifications_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
}

func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261121-mysql-verifications",
		after: []string{"20261117-mysql-changes"},
		steps: mysqlVerificationsSteps,
	})
}

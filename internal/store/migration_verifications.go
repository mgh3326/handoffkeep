package store

// Version 46 adds the verification ledger (hk 3120): one verdict per
// (change, head SHA), saying how that head was verified. It is a new table;
// the foreign key to changes takes SHARE ROW EXCLUSIVE on changes for the
// rest of the transaction, so change writers wait until commit.
var verificationsV46 = []string{
	`CREATE TABLE IF NOT EXISTS verifications (
		id BIGSERIAL PRIMARY KEY,
		change_id BIGINT NOT NULL REFERENCES changes(id),
		head_sha TEXT NOT NULL CHECK(head_sha ~ '^[0-9a-f]{40}$'),
		level TEXT NOT NULL CHECK(level IN ('live-verified','unit-test-verified','type-check-only','verifier-blocked','verifier-failed')),
		verifier TEXT NOT NULL,
		verifier_job_id TEXT,
		family TEXT NOT NULL,
		profile TEXT,
		model TEXT,
		harness TEXT,
		evidence TEXT NOT NULL,
		ci_result TEXT CHECK(ci_result IN ('success','failure','pending','none')),
		source TEXT NOT NULL CHECK(source IN ('direct_merge','mergewake')),
		recorded_by TEXT NOT NULL,
		recorded_at TIMESTAMPTZ NOT NULL,
		UNIQUE(change_id, head_sha)
	)`,
	`INSERT INTO schema_version(version) VALUES (46)`,
}

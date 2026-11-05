package store

// Version 46 lets the operator deny a relayed answer (hk 3163; design hk:doc
// design/2026-11-06/hk-work-record-design-v1.2 §13.1 R1/R2). A relay to a
// human_only request waits as pending_confirmation, and until now nothing
// could record "the operator says this relay is wrong" ([아님]). The deny is
// its own decision_answer_attempts row with result denied, pointing at the
// denied attempt through denies_attempt_id and carrying the operator's
// optional receipt; the denied row itself is never updated. The partial
// unique index allows one deny per attempt, and the CHECK ties the column to
// the result in both directions.
//
// The result CHECK is found by name in pg_constraint (the version 41
// pattern) because version 43 created it unnamed inside CREATE TABLE.
//
// Lock: ACCESS EXCLUSIVE on decision_answer_attempts while the two CHECKs
// validate and the index builds; the table is small. ADD COLUMN works under
// the append-only trigger, which fires on UPDATE and DELETE only.
var decisionDenyV46 = []string{
	`DO $$ DECLARE c text; BEGIN
		FOR c IN SELECT conname FROM pg_constraint WHERE conrelid = 'decision_answer_attempts'::regclass AND contype = 'c' AND conkey = ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid = 'decision_answer_attempts'::regclass AND attname = 'result')] LOOP
			EXECUTE format('ALTER TABLE decision_answer_attempts DROP CONSTRAINT %I', c);
		END LOOP;
	END $$`,
	`ALTER TABLE decision_answer_attempts
		ADD COLUMN IF NOT EXISTS denies_attempt_id BIGINT REFERENCES decision_answer_attempts(id),
		ADD COLUMN IF NOT EXISTS receipt TEXT,
		ADD CONSTRAINT decision_answer_attempts_result_check CHECK(result IN ('answered','conflict','stale_revision','pending_confirmation','expired','superseded','refused','denied')),
		ADD CONSTRAINT decision_answer_attempts_denies_check CHECK((result = 'denied') = (denies_attempt_id IS NOT NULL))`,
	`CREATE UNIQUE INDEX IF NOT EXISTS decision_answer_attempts_one_deny ON decision_answer_attempts(denies_attempt_id) WHERE denies_attempt_id IS NOT NULL`,
	`INSERT INTO schema_version(version) VALUES (46)`,
}

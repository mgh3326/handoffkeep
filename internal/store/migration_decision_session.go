package store

// Version 46 (hk 3064) lets an operator session answer through ResolveDecision
// and keeps every attempt's proposed answer. The channel CHECK is replaced to
// admit 'session'; option_id, text and operator_quote let a pending or
// refused attempt be shown, which payload_hash alone cannot. Each ALTER takes
// ACCESS EXCLUSIVE on decision_answer_attempts until commit; the new CHECK is
// validated in one scan of that small table under the lock, and the nullable
// ADD COLUMNs are catalog-only. The append-only trigger blocks only UPDATE and
// DELETE, so neither statement trips it. ResolveDecision waits meanwhile.
var decisionSessionV46 = []string{
	`ALTER TABLE decision_answer_attempts
		DROP CONSTRAINT decision_answer_attempts_channel_check,
		ADD CONSTRAINT decision_answer_attempts_channel_check
			CHECK(channel IN ('cli','assistant','console','session','linear','lane','default'))`,
	`ALTER TABLE decision_answer_attempts
		ADD COLUMN option_id TEXT,
		ADD COLUMN text TEXT,
		ADD COLUMN operator_quote TEXT`,
	`INSERT INTO schema_version(version) VALUES (46)`,
}

package store

// Version 34 lets task_events record a source event that changed nothing
// (kind source_event, docs/linear-internalize.md rule 2): every mergewake,
// deploy, job and sweep event is recorded, applied or not, and a row that
// moved nothing must not read as a transition. The constraint is replaced
// NOT VALID, so existing rows are not scanned; ALTER TABLE takes a brief
// ACCESS EXCLUSIVE lock on task_events.
var eventsV34 = []string{
	`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project','area','state_migration','awaiting_decision','parent','relation','auto_close','metadata','source_event')) NOT VALID`,
	`INSERT INTO schema_version(version) VALUES (34)`,
}

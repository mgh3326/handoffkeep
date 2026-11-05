package store

// taskClosureV47 adds the completed state and closure shape check. ALTER TABLE
// takes ACCESS EXCLUSIVE on tasks until commit; both validated CHECKs scan tasks
// once under it. Existing rows pass because closure is absent and the state
// list is a superset.
var taskClosureV47 = []string{
	`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged','deployed','dropped','duplicate','icebox','completed'))`,
	`ALTER TABLE tasks ADD CONSTRAINT tasks_closure_check CHECK(refs->'closure' IS NULL OR (refs->'closure'->>'type' IN ('resolved','cancelled','obsolete','superseded') AND COALESCE(refs->'closure'->>'evidence','') <> ''))`,
	`INSERT INTO schema_version(version) VALUES (47)`,
}

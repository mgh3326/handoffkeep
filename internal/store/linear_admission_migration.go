package store

// Version 31 adds durable admission and ownership metadata. The ownership FK
// takes SHARE ROW EXCLUSIVE on tasks. Existing task and legacy link rows stay
// unchanged. For each task, reserve its latest unambiguous issue by updated_at,
// then created_at, then issue id. Multi-task issue ids stay unclaimed. Every
// skipped distinct pair is retained in linear_board_ownership_conflicts.
var linearAdmissionV31 = []string{
	`CREATE TABLE linear_create_admissions(issue_id TEXT PRIMARY KEY,created_at TIMESTAMPTZ NOT NULL,uncertain BOOLEAN NOT NULL DEFAULT true)`,
	`CREATE TABLE linear_board_owners(issue_id TEXT PRIMARY KEY,task_id BIGINT NOT NULL UNIQUE REFERENCES tasks(id))`,
	`CREATE TABLE linear_board_ownership_conflicts(task_id BIGINT NOT NULL,issue_id TEXT NOT NULL,reason TEXT NOT NULL CHECK(reason IN ('ambiguous_issue','additional_task_issue')),observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),PRIMARY KEY(task_id,issue_id))`,
	`CREATE TABLE linear_capacity_state(singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),held BOOLEAN NOT NULL DEFAULT false,policy_limit INT NOT NULL DEFAULT 275)`,
	`INSERT INTO linear_capacity_state(singleton) VALUES(true)`,
	`WITH pairs AS (
 SELECT task_id,issue_id,max(updated_at) AS last_updated,max(created_at) AS last_created FROM linear_issues GROUP BY task_id,issue_id
 ), eligible AS (
 SELECT p.* FROM pairs p WHERE NOT EXISTS(SELECT 1 FROM pairs other WHERE other.issue_id=p.issue_id AND other.task_id<>p.task_id)
 ), ranked AS (
 SELECT *,row_number() OVER(PARTITION BY task_id ORDER BY last_updated DESC NULLS LAST,last_created DESC NULLS LAST,issue_id ASC) AS choice FROM eligible
 ) INSERT INTO linear_board_owners(issue_id,task_id) SELECT issue_id,task_id FROM ranked WHERE choice=1 ORDER BY task_id,issue_id ON CONFLICT DO NOTHING`,
	`INSERT INTO linear_board_ownership_conflicts(task_id,issue_id,reason)
 SELECT DISTINCT l.task_id,l.issue_id,CASE WHEN EXISTS(SELECT 1 FROM linear_issues other WHERE other.issue_id=l.issue_id AND other.task_id<>l.task_id) THEN 'ambiguous_issue' ELSE 'additional_task_issue' END
 FROM linear_issues l WHERE NOT EXISTS(SELECT 1 FROM linear_board_owners o WHERE o.task_id=l.task_id AND o.issue_id=l.issue_id)
 ON CONFLICT DO NOTHING`,
	`INSERT INTO schema_version(version) VALUES(31)`,
}

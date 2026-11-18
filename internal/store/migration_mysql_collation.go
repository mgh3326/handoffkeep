package store

import "strings"

// 20261118-mysql-collation rebuilds every trigger and the two JSON CHECK
// constraints under the pinned connection collation utf8mb4_0900_bin.
//
// Why: #494 pinned collation_connection to utf8mb4_0900_bin (design §11.4),
// but a trigger keeps the collation_connection it was created with, so every
// trigger created before #494 still evaluates its literals (and its CAST AS
// CHAR results) in the server's case-insensitive default. Any comparison of
// such a string with a utf8mb4_0900_bin column or literal is error 1267.
// JSON strings never followed the connection at all: JSON_UNQUOTE and
// JSON_TYPE carry utf8mb4_bin, so the closure and skills CHECK constraints
// compared utf8mb4_bin with utf8mb4_0900_bin on every write (hk 5169).
//
// The rebuild drops each trigger and creates it again from the same body its
// owning migration registers, so old and new definitions cannot drift: the
// steps below are derived from those slices, with the IF NOT EXISTS infix
// stripped so a stale body can never survive behind a no-op create. The two
// CHECK constraints are dropped and re-added from the shared predicate
// consts. The quota_history_immutable_* triggers are deliberately absent:
// 20261117-mysql-bulk-guards dropped them, so there is nothing to rebuild.
//
// Locks: DROP/CREATE TRIGGER takes a metadata lock on its table while it
// runs, and the ALTERs take one on tasks and jobs; each trigger is gone only
// for its own rebuild. There are no MySQL production writers yet, so no
// writer waits. Expand-only: no table, column or constraint the previous
// release uses is dropped or narrowed; the rebuilt objects keep their names
// and semantics, and the previous release runs on the same 0900_bin
// connection, so the rebuilt bodies evaluate exactly as before for it.

// mysqlCollationTriggerName returns the trigger a CREATE TRIGGER step
// creates, or false when the step creates no trigger.
func mysqlCollationTriggerName(step string) (string, bool) {
	rest, ok := strings.CutPrefix(step, "CREATE TRIGGER IF NOT EXISTS ")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, " ")
	if name == "" {
		return "", false
	}
	return name, true
}

// mysqlCollationDropName returns the trigger a DROP TRIGGER step drops, or
// false when the step drops no trigger.
func mysqlCollationDropName(step string) (string, bool) {
	name, ok := strings.CutPrefix(step, "DROP TRIGGER IF EXISTS ")
	if !ok || name == "" || strings.Contains(name, " ") {
		return "", false
	}
	return name, true
}

// mysqlCollationRecreateSteps returns the DROP + CREATE pair rebuilding every
// trigger in groups. The CREATE drops the IF NOT EXISTS infix: the DROP just
// removed the trigger, and a bare CREATE fails loudly if it somehow still
// exists instead of silently keeping the stale body. A step that creates no
// trigger panics: the registry validity test would fail the build anyway,
// and a silent skip would leave a stale trigger behind.
func mysqlCollationRecreateSteps(groups ...[]string) []string {
	const prefix = "CREATE TRIGGER IF NOT EXISTS "
	var out []string
	for _, steps := range groups {
		for _, step := range steps {
			name, ok := mysqlCollationTriggerName(step)
			if !ok {
				panic("mysql collation migration: trigger step without a name: " + step)
			}
			out = append(out,
				"DROP TRIGGER IF EXISTS "+name,
				"CREATE TRIGGER "+strings.TrimPrefix(step, prefix),
			)
		}
	}
	return out
}

// mysqlCollationCheckSteps rebuilds the two JSON CHECK constraints from the
// shared predicate consts, so the constraint text cannot drift from the
// owning migrations' definitions.
var mysqlCollationCheckSteps = []string{
	mysqlIfConstraint("tasks", "tasks_closure_check", `ALTER TABLE tasks DROP CHECK tasks_closure_check`),
	mysqlIfNoConstraint("tasks", "tasks_closure_check", `ALTER TABLE tasks ADD CONSTRAINT tasks_closure_check CHECK(`+mysqlTasksClosureCheckBody+`)`),
	mysqlIfConstraint("jobs", "jobs_skills_check", `ALTER TABLE jobs DROP CHECK jobs_skills_check`),
	mysqlIfNoConstraint("jobs", "jobs_skills_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_skills_check CHECK(`+mysqlJobsSkillsCheckBody+`)`),
}

func init() {
	registerMySQLMigration(mysqlMigration{
		id: "20261118-mysql-collation",
		after: []string{
			"20261117-mysql-bulk-guards",
			"20261117-mysql-comments",
			"20261117-mysql-changes",
			"20261118-mysql-tasks",
			"20261118-mysql-relay-jobs",
			"20261118-mysql-decisions",
			"20261118-decision-sync-bulk-load",
			"20261118-mysql-documents",
			"20261118-mysql-yt-feed",
			"20261118-mysql-yt-write-ops",
		},
		steps: append(append([]string{}, mysqlCollationCheckSteps...),
			mysqlCollationRecreateSteps(
				mysqlBulkGuardTriggers,
				mysqlCommentsTriggers,
				mysqlChangesTriggers,
				mysqlTasksTriggers,
				mysqlDecisionsTriggers,
				mysqlDecisionSyncTriggers,
				mysqlDocumentsTriggers,
				mysqlYTFeedTriggers,
				mysqlYTOpsTriggers,
			)...),
	})
}

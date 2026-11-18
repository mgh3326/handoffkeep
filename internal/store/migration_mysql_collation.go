package store

import (
	"sort"
	"strings"
)

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
// The rebuild derives the triggers dynamically from the live trigger set
// across all registered migrations (orderMySQLMigrations), so a trigger
// modified or added by a parallel or future migration cannot be missed.
// The steps strip any IF NOT EXISTS infix so a stale body can never survive
// behind a no-op create. The two CHECK constraints are dropped and re-added
// from the shared predicate consts.
//
// Locks: DROP/CREATE TRIGGER takes a metadata lock on its table while it
// runs, and the ALTERs take one on tasks and jobs; each trigger is gone only
// for its own rebuild. There are no MySQL production writers yet, so no
// writer waits. Expand-only: no table, column or constraint the previous
// release uses is dropped or narrowed; the rebuilt objects keep their names
// and semantics, and the previous release runs on the same 0900_bin
// connection, so the rebuilt bodies evaluate exactly as before for it.

const mysqlCollationMigrationID = "20261118-mysql-collation"

// mysqlCollationTriggerName returns the trigger a CREATE TRIGGER step
// creates, or false when the step creates no trigger.
func mysqlCollationTriggerName(step string) (string, bool) {
	step = strings.TrimSpace(step)
	rest, ok := strings.CutPrefix(step, "CREATE TRIGGER ")
	if !ok {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
	name, _, ok := strings.Cut(rest, " ")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// mysqlCollationDropName returns the trigger a DROP TRIGGER step drops, or
// false when the step drops no trigger.
func mysqlCollationDropName(step string) (string, bool) {
	step = strings.TrimSpace(step)
	rest, ok := strings.CutPrefix(step, "DROP TRIGGER ")
	if !ok {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "IF EXISTS ")
	name, _, _ := strings.Cut(rest, " ")
	name = strings.TrimSuffix(name, ";")
	if name == "" {
		return "", false
	}
	return name, true
}

// mysqlBareCreateTrigger returns the trigger name and bare CREATE TRIGGER
// statement (stripping IF NOT EXISTS), or false when step is not a create trigger.
func mysqlBareCreateTrigger(step string) (name, bare string, ok bool) {
	step = strings.TrimSpace(step)
	const prefix = "CREATE TRIGGER "
	rest, ok := strings.CutPrefix(step, prefix)
	if !ok {
		return "", "", false
	}
	rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
	name, _, ok = strings.Cut(rest, " ")
	if !ok || name == "" {
		return "", "", false
	}
	return name, prefix + rest, true
}

// mysqlLiveTriggers derives the live triggers and their bare CREATE statements
// from the migrations, in topological dependency order (excluding the collation
// migration itself). It also returns the set of migration IDs that define or
// replace those live triggers.
func mysqlLiveTriggers(migrations []mysqlMigration) (live map[string]string, owners map[string]string, err error) {
	others := make([]mysqlMigration, 0, len(migrations))
	for _, m := range migrations {
		if m.id != mysqlCollationMigrationID {
			others = append(others, m)
		}
	}
	ordered, err := orderMySQLMigrations(others)
	if err != nil {
		return nil, nil, err
	}
	live = make(map[string]string)
	owners = make(map[string]string)
	for _, m := range ordered {
		for _, step := range m.steps {
			if name, ok := mysqlCollationDropName(step); ok {
				delete(live, name)
				delete(owners, name)
			}
			if name, bare, ok := mysqlBareCreateTrigger(step); ok {
				live[name] = bare
				owners[name] = m.id
			}
		}
	}
	return live, owners, nil
}

// resolveMySQLCollationMigration returns a caller-owned copy of migrations where
// the collation migration's recreate steps and after dependencies are derived
// from the live triggers across all other migrations. It never mutates the
// input slice or the global mysqlMigrations registry.
func resolveMySQLCollationMigration(migrations []mysqlMigration) ([]mysqlMigration, error) {
	targetIdx := -1
	for i := range migrations {
		if migrations[i].id == mysqlCollationMigrationID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		out := make([]mysqlMigration, len(migrations))
		copy(out, migrations)
		return out, nil
	}
	live, owners, err := mysqlLiveTriggers(migrations)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(live))
	for name := range live {
		names = append(names, name)
	}
	sort.Strings(names)

	steps := make([]string, 0, len(mysqlCollationCheckSteps)+len(names)*2)
	steps = append(steps, mysqlCollationCheckSteps...)
	for _, name := range names {
		steps = append(steps,
			"DROP TRIGGER IF EXISTS "+name,
			live[name],
		)
	}

	present := map[string]bool{}
	for _, m := range migrations {
		present[m.id] = true
	}
	afterMap := map[string]bool{}
	if present["20261118-mysql-tasks"] {
		afterMap["20261118-mysql-tasks"] = true
	}
	if present["20261118-mysql-relay-jobs"] {
		afterMap["20261118-mysql-relay-jobs"] = true
	}
	for _, migID := range owners {
		if present[migID] {
			afterMap[migID] = true
		}
	}
	after := make([]string, 0, len(afterMap))
	for id := range afterMap {
		after = append(after, id)
	}
	sort.Strings(after)

	out := make([]mysqlMigration, len(migrations))
	copy(out, migrations)
	out[targetIdx].steps = steps
	out[targetIdx].after = after
	return out, nil
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
		id:    mysqlCollationMigrationID,
		after: []string{"20261118-mysql-tasks", "20261118-mysql-relay-jobs"},
		steps: append([]string{}, mysqlCollationCheckSteps...),
	})
}

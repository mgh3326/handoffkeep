package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Helpers of the tasks core (store.go task functions, model_project.go,
// model_milestone.go) that only need a MySQL text beside the PostgreSQL one.
// Names end in Core so another area's helper never collides with them.

// coreTaskMutationLock is the task-mutation advisory lock of lockorder.go
// level 2; lockTx maps it to an hk_locks row on MySQL.
const coreTaskMutationLock int64 = 8241873001

// updateTaskReturningCore runs a single-row UPDATE of tasks and reads the new
// row into x. PostgreSQL appends its RETURNING list to pgSQL; MySQL has no
// RETURNING, so it runs mySQL and reads the row back by id in the same
// transaction, whose row lock (or the task-mutation lock) keeps the read
// current. A row that does not exist is ErrNoRows on both.
func (s *Store) updateTaskReturningCore(ctx context.Context, tx transaction, x *Task, id int64, pgSQL string, pgArgs []any, mySQL string, myArgs []any) error {
	if s.dialect != mysqlDialect {
		return scanTask(tx.QueryRow(ctx, pgSQL+taskColumns, pgArgs...), x)
	}
	n, err := tx.Exec(ctx, mySQL, myArgs...)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoRows
	}
	return scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, id), x)
}

// stateClauseCore is stateClause for MySQL: an IN list instead of = ANY(array).
func stateClauseCore(state string, args *[]any) string {
	f, _ := ParseStateFilter(state)
	if f.Awaiting {
		return " AND awaiting_decision"
	}
	for _, st := range f.States {
		*args = append(*args, st)
	}
	return " AND state IN (" + placeholders(len(f.States)) + ")"
}

// coreTaskActivitySQL is taskActivitySQL for MySQL. GREATEST is NULL there when
// any argument is NULL, where PostgreSQL ignores NULLs, so each optional
// subquery falls back to created_at, which is already one of the arguments.
// job_id is read as a JSON string (a JSON null is not a job id).
const coreTaskActivitySQL = `GREATEST(tasks.created_at,
	COALESCE((SELECT max(e.at) FROM task_events e WHERE e.task_id=tasks.id), tasks.created_at),
	COALESCE((SELECT max(c.created_at) FROM task_comments c WHERE c.task_id=tasks.id), tasks.created_at),
	COALESCE((SELECT max(j.updated_at) FROM jobs j WHERE j.job_id=IF(JSON_TYPE(JSON_EXTRACT(tasks.refs,'$.job_id'))='STRING',JSON_UNQUOTE(JSON_EXTRACT(tasks.refs,'$.job_id')),NULL) AND j.task_id=tasks.id), tasks.created_at))`

// coreTaskEventColumnsMySQL is taskEventColumns with the reserved words
// from/to/by quoted with backticks instead of double quotes.
func coreTaskEventColumnsMySQL() string {
	return strings.NewReplacer(`"from"`, "`from`", `"to"`, "`to`", `"by"`, "`by`").Replace(taskEventColumns)
}

// coreJSONString is v as a JSON string literal, for CAST(? AS JSON) comparisons.
func coreJSONString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// projectUpdateArgsCore orders the arguments of the task_projects metadata
// UPDATE: $1..$4 are name, status, start, target; the MySQL text lists name last.
func projectUpdateArgsCore(d storeDialect, name, status string, start, target *time.Time) []any {
	if d == mysqlDialect {
		return []any{status, start, target, name}
	}
	return []any{name, status, start, target}
}

// knownTaskLanesMySQLTx is knownTaskLanesTx on MySQL. The statements run one
// after the other so a lane that tasks already use never reads relay_events or
// chat_questions (tables of other areas that may not exist yet).
func knownTaskLanesMySQLTx(ctx context.Context, tx transaction, name string) (bool, error) {
	for _, q := range []string{
		`SELECT EXISTS(SELECT 1 FROM tasks WHERE lane=?)`,
		`SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_lane=?)`,
		`SELECT EXISTS(SELECT 1 FROM relay_events WHERE owner_lane=?)`,
		`SELECT EXISTS(SELECT 1 FROM chat_questions WHERE lane=?)`,
	} {
		var known bool
		if err := tx.QueryRow(ctx, q, name).Scan(&known); err != nil {
			return false, err
		}
		if known {
			return true, nil
		}
	}
	return false, nil
}

package store

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5"
)

// MySQL support shared by the report and read files of the tasks area (hk 5074,
// A3): frontier, drift, hygiene, settle, task_reads and task_automation. Names
// here are private to those files.

// reportJSONText is the MySQL text of PostgreSQL's col->>'a'->>'b' for a path
// such as '$.decision_request.status': the string at the path, and SQL NULL when
// the path is absent or holds a JSON null or a non-string. (JSON_UNQUOTE alone
// would answer the text 'null' for a JSON null.)
//
// JSON strings carry utf8mb4_bin, which no longer mixes with the connection's
// utf8mb4_0900_bin (design §11.4, hk 5169), so both JSON calls are pinned to
// the table collation.
func reportJSONText(col, path string) string {
	x := "JSON_EXTRACT(" + col + ",'" + path + "')"
	return "IF(JSON_TYPE(" + x + ") COLLATE utf8mb4_0900_bin='STRING',JSON_UNQUOTE(" + x + ") COLLATE utf8mb4_0900_bin,NULL)"
}

// reportPRURLSQL is prURLSQL for MySQL: the PR URL of changes row c.
const reportPRURLSQL = `CONCAT('https://github.com/',c.repo,'/pull/',CAST(c.pr_number AS CHAR))`

// reportDecisionOpenSQL is decisionOpenSQL for MySQL. The read time appears
// twice, so the caller passes it twice.
const reportDecisionOpenSQL = `status='open' AND (expires_at IS NULL OR expires_at>?) AND (snoozed_until IS NULL OR snoozed_until<=?)`

// reportDecisionEffectiveSQL is decisionEffectiveStatusSQL for MySQL, with the
// read time as its one placeholder.
const reportDecisionEffectiveSQL = `CASE WHEN d.status='open' AND d.expires_at<=? THEN 'expired' ELSE d.status END`

// taskActivitySQLMySQL is taskActivitySQL for MySQL. GREATEST yields NULL on
// MySQL when any argument is NULL, so each missing term falls back to
// created_at, the floor the PostgreSQL expression also ends at.
const taskActivitySQLMySQL = `GREATEST(tasks.created_at,
	COALESCE((SELECT max(e.at) FROM task_events e WHERE e.task_id=tasks.id), tasks.created_at),
	COALESCE((SELECT max(c.created_at) FROM task_comments c WHERE c.task_id=tasks.id), tasks.created_at),
	COALESCE((SELECT max(j.updated_at) FROM jobs j WHERE j.job_id=JSON_UNQUOTE(JSON_EXTRACT(tasks.refs,'$.job_id')) AND j.task_id=tasks.id), tasks.created_at))`

// activitySQLFor is the task activity expression for the dialect of x, a
// querier or transaction (it reads the row as "tasks").
func activitySQLFor(x any) string {
	return sqlFor(x, taskActivitySQL, taskActivitySQLMySQL)
}

// pickArgs picks the argument list of the dialect's statement text: MySQL's
// positional texts repeat a value where PostgreSQL reuses one numbered
// placeholder, or take their arguments in another order.
func pickArgs(s *Store, pg, my []any) []any {
	if s.dialect == mysqlDialect {
		return my
	}
	return pg
}

// reportIDArgs turns ids into statement arguments for an IN list.
func reportIDArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// beginReportRead starts a read-only transaction. repeatable asks for one
// snapshot across its statements: REPEATABLE READ on both dialects (the MySQL
// read view starts at the first consistent read, so one is issued here). Without
// it the transaction runs at the dialect's default isolation, as the PostgreSQL
// callers did before. The caller defers Rollback and ends with Commit.
func (s *Store) beginReportRead(ctx context.Context, repeatable bool) (transaction, error) {
	if s.dialect != mysqlDialect {
		opts := pgx.TxOptions{AccessMode: pgx.ReadOnly}
		if repeatable {
			opts.IsoLevel = pgx.RepeatableRead
		}
		tx, err := s.pool.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return pgTransaction{tx}, nil
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if repeatable {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db().(sqlQuerier).db.BeginTx(ctx, opts)
	if err != nil {
		return nil, mapDriverError(err)
	}
	if repeatable {
		if _, err = tx.ExecContext(ctx, `SELECT 1 FROM task_projects LIMIT 1`); err != nil {
			_ = tx.Rollback()
			return nil, mapDriverError(err)
		}
	}
	return sqlTransaction{tx}, nil
}

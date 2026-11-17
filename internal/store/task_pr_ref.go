package store

import (
	"context"
	"regexp"
	"strconv"
)

// The PR a task is delivered by is one full PR URL, so the deploy rule can
// match it (hk 4844). Two rules keep it so: a write normalises a bare PR
// number to the project's repo (normalisePRRefTx), and a bundle child with
// no PR of its own takes its bundle parent's (taskDeliveredPRSQL).

// barePRDigitsRE is refs.pr written as digits alone. A PR number is at most
// nine digits; a longer run of digits is refused, not stored as text.
var barePRDigitsRE = regexp.MustCompile(`^[0-9]+$`)

// normalisePRRefTx returns the refs.pr a task of project stores for pr. A
// full URL is canonicalised as before. A bare number takes the one repo the
// project's PRs live in: the repos of its tasks' change records and of the PR
// URLs its tasks name. Zero repos, or more than one, refuses the write, since
// a number alone would name the wrong PR. A task with no project has none.
func normalisePRRefTx(ctx context.Context, tx transaction, project *string, pr string) (string, error) {
	if !barePRDigitsRE.MatchString(pr) {
		return canonicalPRURL(pr), nil
	}
	n, err := strconv.Atoi(pr)
	if len(pr) > 9 || err != nil || n < 1 {
		return "", invalidChangeWrite("a PR number is a positive integer of at most nine digits")
	}
	query, args := `SELECT repo FROM (
	SELECT c.repo FROM change_tasks ct JOIN changes c ON c.id=ct.change_id JOIN tasks t ON t.id=ct.task_id WHERE t.project=$1
	UNION
	SELECT substring(t.refs->>'pr' FROM '^https://github[.]com/([^/]+/[^/]+)/pull/[0-9]+$') FROM tasks t WHERE t.project=$1
	) r WHERE repo IS NOT NULL`, []any{project}
	if dialectFor(tx) == mysqlDialect {
		// MySQL has no capture-group substring: REGEXP_REPLACE keeps group 1 of a
		// refs.pr that matches the whole pattern. The cast pins the collation both
		// UNION branches share. B5 dependency: changes and change_tasks.
		query, args = `SELECT repo FROM (
	SELECT c.repo FROM change_tasks ct JOIN changes c ON c.id=ct.change_id JOIN tasks t ON t.id=ct.task_id WHERE t.project=?
	UNION
	SELECT CAST(REGEXP_REPLACE(p.pr, '^https://github[.]com/([^/]+/[^/]+)/pull/[0-9]+$', '$1') AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_0900_bin
	FROM (SELECT `+mysqlRefsPRSQL+` AS pr FROM tasks t WHERE t.project=?) p
	WHERE p.pr REGEXP '^https://github[.]com/[^/]+/[^/]+/pull/[0-9]+$'
	) r WHERE repo IS NOT NULL`, []any{project, project}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return "", err
	}
	repos, err := collectRows(rows, rowTo[string])
	if err != nil {
		return "", err
	}
	if len(repos) != 1 {
		return "", invalidChangeWrite("a bare PR number needs the one repo the project's PRs live in; this project has %d, so give the full PR URL", len(repos))
	}
	return changeURL(repos[0], n), nil
}

// taskDeliveredPRSQL is the PR that delivers the task aliased t: its own
// refs.pr, else the refs.pr of its bundle parent. Only a parent titled
// "[bundle ...]" passes its PR down (robin-mode §Work boundary), so an epic's
// PR never delivers a child its own change did not land. The empty string is
// "none".
const taskDeliveredPRSQL = `COALESCE(NULLIF(t.refs->>'pr',''), (SELECT p.refs->>'pr' FROM tasks p WHERE p.id=t.parent_id AND p.title LIKE '[bundle %'), '')`

// mysqlRefsPRSQL reads refs.pr of the task aliased t as a string; a missing
// key and a JSON null are both SQL NULL, as PostgreSQL's ->> gives.
const mysqlRefsPRSQL = `IF(JSON_TYPE(JSON_EXTRACT(t.refs,'$.pr'))='STRING', JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.pr')), NULL)`

// taskDeliveredPRMySQLSQL is taskDeliveredPRSQL for MySQL.
const taskDeliveredPRMySQLSQL = `COALESCE(NULLIF(` + mysqlRefsPRSQL + `,''), (SELECT IF(JSON_TYPE(JSON_EXTRACT(p.refs,'$.pr'))='STRING', JSON_UNQUOTE(JSON_EXTRACT(p.refs,'$.pr')), NULL) FROM tasks p WHERE p.id=t.parent_id AND p.title LIKE '[bundle %'), '')`

// deliveredPRTx is taskDeliveredPRSQL for one task.
func deliveredPRTx(ctx context.Context, tx transaction, taskID int64) (string, error) {
	var pr string
	err := tx.QueryRow(ctx, sqlFor(tx, `SELECT `+taskDeliveredPRSQL+` FROM tasks t WHERE t.id=$1`, `SELECT `+taskDeliveredPRMySQLSQL+` FROM tasks t WHERE t.id=?`), taskID).Scan(&pr)
	return pr, err
}

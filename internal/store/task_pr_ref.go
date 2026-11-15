package store

import (
	"context"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
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
func normalisePRRefTx(ctx context.Context, tx pgx.Tx, project *string, pr string) (string, error) {
	if !barePRDigitsRE.MatchString(pr) {
		return canonicalPRURL(pr), nil
	}
	n, err := strconv.Atoi(pr)
	if len(pr) > 9 || err != nil || n < 1 {
		return "", invalidChangeWrite("a PR number is a positive integer of at most nine digits")
	}
	rows, err := tx.Query(ctx, `SELECT repo FROM (
	SELECT c.repo FROM change_tasks ct JOIN changes c ON c.id=ct.change_id JOIN tasks t ON t.id=ct.task_id WHERE t.project=$1
	UNION
	SELECT substring(t.refs->>'pr' FROM '^https://github[.]com/([^/]+/[^/]+)/pull/[0-9]+$') FROM tasks t WHERE t.project=$1
	) r WHERE repo IS NOT NULL`, project)
	if err != nil {
		return "", err
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[string])
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

// deliveredPRTx is taskDeliveredPRSQL for one task.
func deliveredPRTx(ctx context.Context, tx pgx.Tx, taskID int64) (string, error) {
	var pr string
	err := tx.QueryRow(ctx, `SELECT `+taskDeliveredPRSQL+` FROM tasks t WHERE t.id=$1`, taskID).Scan(&pr)
	return pr, err
}

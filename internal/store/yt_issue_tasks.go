package store

import (
	"context"
	"strconv"
)

// YTIssueTask is the task a YouTrack issue belongs to, and how strongly hk
// knows it: a lower Rank is the stronger link.
type YTIssueTask struct {
	TaskID int64
	Rank   int
}

// The ranks of YTIssueTaskIDs, strongest first. The console ranks an issue's
// own hk ID field after these (hk 4430).
const (
	YTLinkBinding   = 1 // yt_task_bindings: the verified binding (hk 3810)
	YTLinkMirror    = 2 // task_origins provider youtrack: the mirror link
	YTLinkEntityMap = 3 // yt_entity_map task row: an applied issue_create op
)

// ytIssueTaskIDsSQL unions the three links, each naming its rank. The tenant is
// one parameter, bound in each branch.
const ytIssueTaskIDsSQL = `SELECT issue_id, task_id::text, 1 FROM yt_task_bindings WHERE tenant=$1
		UNION ALL
		SELECT external_id, task_id::text, 2 FROM task_origins WHERE provider='youtrack' AND (coalesce(tenant,'')='' OR tenant=$1)
		UNION ALL
		SELECT yt_id, hk_key, 3 FROM yt_entity_map WHERE entity_kind='task' AND (tenant=$1 OR tenant='')`

// ytIssueTaskIDsMySQL is the MySQL twin: the tenant is one ? per branch, and
// task_id is cast with CAST(... AS CHAR) instead of ::text.
const ytIssueTaskIDsMySQL = `SELECT issue_id, CAST(task_id AS CHAR), 1 FROM yt_task_bindings WHERE tenant=?
		UNION ALL
		SELECT external_id, CAST(task_id AS CHAR), 2 FROM task_origins WHERE provider='youtrack' AND (coalesce(tenant,'')='' OR tenant=?)
		UNION ALL
		SELECT yt_id, hk_key, 3 FROM yt_entity_map WHERE entity_kind='task' AND (tenant=? OR tenant='')`

// YTIssueTaskIDs maps every YouTrack issue id hk links to a task in tenant to
// that task, from the binding, the mirror link and the write protocol's
// entity map. When two sources name an issue, the stronger one is kept.
// Mirror links with no tenant are read as the tenant's, and so are map rows a
// release that predates the tenant column wrote (hk 3912): the same YouTrack
// id on another tenant is another issue, so a row with a tenant is read only
// on it. One plain read, no lock: the console joins it to a YouTrack read that
// is not transactional.
func (s *Store) YTIssueTaskIDs(ctx context.Context, tenant string) (map[string]YTIssueTask, error) {
	query, args := sqlFor(s, ytIssueTaskIDsSQL, ytIssueTaskIDsMySQL), []any{tenant, tenant, tenant}
	if dialectFor(s) != mysqlDialect {
		args = []any{tenant}
	}
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]YTIssueTask{}
	for rows.Next() {
		var issueID, key string
		var rank int
		if err := rows.Scan(&issueID, &key, &rank); err != nil {
			return nil, err
		}
		taskID, err := strconv.ParseInt(key, 10, 64)
		if err != nil || taskID < 1 || issueID == "" {
			continue
		}
		if have, ok := out[issueID]; ok && have.Rank <= rank {
			continue
		}
		out[issueID] = YTIssueTask{TaskID: taskID, Rank: rank}
	}
	return out, rows.Err()
}

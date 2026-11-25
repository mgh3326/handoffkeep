package store

// 20261125-settled-open-decision-withdraw on MySQL, the twin of the PostgreSQL
// migration of the same ID (hk 6339). It withdraws each open decision request
// of a settled task and supersedes its open inbox row, in that order: the
// inbox match reads the open refs, which the second statement changes.
//
// Idempotent: a second run matches no row. Expand-only: it changes values and
// adds one JSON key that the previous release does not read; no column or
// table changes. Locks: row locks on the matched decision_requests and tasks
// rows only, under the statement's own scan.
func init() {
	registerMySQLMigration(mysqlMigration{
		id: "20261125-settled-open-decision-withdraw",
		steps: []string{
			`UPDATE decision_requests d JOIN tasks t ON JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.id')) COLLATE utf8mb4_0900_bin = d.source_ref
				SET d.status='superseded', d.superseded_by='withdrawn', d.updated_at=NOW(6)
				WHERE d.source IN ('task_request','linear') AND d.status='open'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.status'))='open'`,
			`UPDATE tasks SET refs = JSON_SET(refs, '$.decision_request.status', 'withdrawn', '$.decision_request.withdrawn_reason', 'task_settled')
				WHERE state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(refs,'$.decision_request.status'))='open'`,
		},
	})
}

package store

// 20261125-settled-open-decision-withdraw on MySQL, the twin of the PostgreSQL
// migration of the same ID (hk 6339). Its steps run in the same order: the
// inbox request and the task_flag rows, the dots operator questions, then the
// task refs and block. Each match reads the open refs, which the last step
// changes. JSON-derived strings carry utf8mb4_bin, so each is compared under an
// explicit collation: a bare comparison with a literal or column is error 1267
// (hk 5169).
//
// Idempotent: a second run matches no row. Expand-only: it changes values and
// adds one JSON key that the previous release does not read; no column or
// table changes. Locks: row locks on the matched decision_requests,
// dots_results and tasks rows only, under each statement's own scan.
func init() {
	registerMySQLMigration(mysqlMigration{
		id: "20261125-settled-open-decision-withdraw",
		steps: []string{
			`UPDATE decision_requests d JOIN tasks t ON d.source_ref COLLATE utf8mb4_bin = JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.id'))
				SET d.status='superseded', d.superseded_by='withdrawn', d.updated_at=NOW(6)
				WHERE d.source IN ('task_request','linear') AND d.status='open'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.status')) COLLATE utf8mb4_0900_bin='open'`,
			`UPDATE decision_requests d JOIN tasks t ON d.task_id = t.id
				SET d.status='superseded', d.superseded_by=CONCAT('task:', t.id, ':', t.state), d.updated_at=NOW(6)
				WHERE d.source='task_flag' AND d.status='open'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.status')) COLLATE utf8mb4_0900_bin='open'`,
			`UPDATE dots_results d JOIN tasks t ON d.task_id = t.id
				SET d.answered_at=NOW(6), d.answered_by='task_settled'
				WHERE d.kind='QUESTION' AND d.ask_operator AND d.answered_at IS NULL
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.decision_request.status')) COLLATE utf8mb4_0900_bin='open'`,
			`UPDATE tasks SET awaiting_decision=false,
				refs = JSON_SET(refs, '$.decision_request.status', 'withdrawn', '$.decision_request.withdrawn_reason', 'task_settled')
				WHERE state IN ('merged','deployed','completed','dropped','duplicate')
				  AND JSON_UNQUOTE(JSON_EXTRACT(refs,'$.decision_request.status')) COLLATE utf8mb4_0900_bin='open'`,
		},
	})
}

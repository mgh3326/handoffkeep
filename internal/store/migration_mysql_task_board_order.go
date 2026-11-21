package store

// 20261121-task-board-order adds (project, state, id) on tasks for the id-cursor
// task list of the console board (hk 5928; digest sample 10, 74,836 calls at
// about 5.8k rows examined each). The board (internal/ui/board.go, boardTasks)
// pages `WHERE id>? AND state IN (?) AND project=? ORDER BY id LIMIT ?`. The
// existing task indexes lead with lane or parent_lane, which this query does
// not filter on, or with priority, which it does not order by, so the read
// walked the table. With the two equality columns first and id last, the read
// is one range of the matching tasks, already in id order, and LIMIT stops it
// after the page (at most 501 rows). The board also pages with state alone or
// with no filter; those forms are not in the digest's top samples and this index
// does not serve them.
//
// Expand-only: one secondary index on a table of about 6k rows, no column or
// constraint change. The build is online for InnoDB; the statement takes a
// short metadata lock at its start and end. The index is restartable through
// mysqlIfNoIndex.
var mysqlTaskBoardOrderSteps = []string{
	mysqlIfNoIndex("tasks", "tasks_project_state_id", `CREATE INDEX tasks_project_state_id ON tasks(project, state, id)`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261121-task-board-order", after: []string{"20261118-mysql-tasks"}, steps: mysqlTaskBoardOrderSteps})
}

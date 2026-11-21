package store

// 20261121-task-priority-order indexes the default task list order (hk 5879).
// ListTasksFiltered without a lane filter sorts every task by priority DESC,
// created_at ASC, id ASC and takes the first page: the only task index that
// covers priority is (lane, state, priority, created_at, id), which needs a
// lane equality to be used. This index lets the planner read the page in order
// and stop. It serves only that default order: the id-cursor board read (digest
// sample 10) orders by id and is served by 20261121-task-board-order instead.
// The tasks table is small (about 6k rows), so the build's metadata lock is
// brief; the index covers every row and nothing is dropped.

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261121-task-priority-order", after: []string{"20261118-mysql-tasks"}, steps: []string{
		mysqlIfNoIndex("tasks", "tasks_priority_order", `CREATE INDEX tasks_priority_order ON tasks(priority DESC, created_at ASC, id ASC)`),
	}})
}

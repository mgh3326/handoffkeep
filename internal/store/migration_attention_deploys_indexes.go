package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261125-attention-deploys-indexes serves the console attention and deploys
// reads (hk 6397; the hk 5584 digests show each scanning the whole tasks
// table about every 86 s). Its MySQL counterpart carries the same ID in
// migration_mysql_attention_deploys_indexes.go.
//
//   - tasks_awaiting_decision_id (awaiting_decision, id): the awaiting half
//     of the attention reads (attention.go, attention_mysql.go) filters
//     awaiting_decision = true, then JSON and triage predicates. The existing
//     tasks keys lead with lane or parent_lane, which these reads do not
//     filter on, so the half walked the table; the new key reads only the
//     awaiting rows.
//   - tasks_state_id (state, id): the stalled half filters state IN
//     ('researching','developing','reviewing'). MySQL already has this key
//     (20261124-mysql-read-indexes); PostgreSQL has none leading with state,
//     so the half walked the table here too.
//   - task_events_kind_to_at_id (kind, "to", at DESC, id DESC):
//     ListMergedTaskEvents (deploys.go) now drives from the merge transitions,
//     newest first, and stops after its limit. No key leads with kind, so the
//     outer read walked task_events; the new key reads only the newest merges.
//
// Expand-only: three secondary indexes, no column, table or constraint change.
// The previous release's reads and writes are unchanged; its plans can only
// gain a key, and writes to tasks and task_events maintain one more each.
// Lock: a plain CREATE INDEX takes SHARE on its table for the whole build, so
// writes to that table wait while it runs and reads continue. CREATE INDEX
// CONCURRENTLY is not available: every registered Apply runs inside the
// migrating transaction, and CONCURRENTLY refuses to run in one. tasks is
// small (about 6k rows), so its two builds are short. task_events grows with
// every transition and is append-only, so its build scan scales with the
// table; check its row count before deploy.
func init() {
	registerMigration(migration{ID: "20261125-attention-deploys-indexes", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE INDEX IF NOT EXISTS tasks_awaiting_decision_id ON tasks(awaiting_decision, id)`,
			`CREATE INDEX IF NOT EXISTS tasks_state_id ON tasks(state, id)`,
			`CREATE INDEX IF NOT EXISTS task_events_kind_to_at_id ON task_events(kind, "to", at DESC, id DESC)`,
		})
	}})
}

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261125-settled-open-decision-withdraw is the backfill for hk 6339. A task
// that settled before withdrawSettledRequestTx still refs an open decision
// request, and drift reports it every day to a lane that cannot close it.
// Each such request is withdrawn with withdrawn_reason task_settled, and its
// open inbox row is superseded as a withdraw supersedes it. The inbox row
// first: its match reads the open refs, which the second statement changes.
//
// Idempotent: a second run matches no row, so it changes nothing.
//
// Expand-only: it changes values and adds one JSON key (withdrawn_reason),
// which the previous release does not read. It writes only status values the
// previous release already handles (withdrawn, superseded). Locks: ROW
// EXCLUSIVE on decision_requests and tasks, and row locks on the matched rows
// only. Each statement scans its table without an index on refs, so the
// scan holds no lock on rows that do not match.
func init() {
	registerMigration(migration{ID: "20261125-settled-open-decision-withdraw", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`UPDATE decision_requests d SET status='superseded',superseded_by='withdrawn',updated_at=now()
				FROM tasks t
				WHERE d.source IN ('task_request','linear') AND d.status='open'
				  AND d.source_ref = t.refs->'decision_request'->>'id'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND t.refs->'decision_request'->>'status' = 'open'`,
			`UPDATE tasks SET refs = jsonb_set(refs, '{decision_request}', (refs->'decision_request') || '{"status":"withdrawn","withdrawn_reason":"task_settled"}'::jsonb)
				WHERE state IN ('merged','deployed','completed','dropped','duplicate')
				  AND refs->'decision_request'->>'status' = 'open'`,
		})
	}})
}

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261125-settled-open-decision-withdraw is the backfill for hk 6339. A task
// that settled before withdrawSettledRequestTx still refs an open decision
// request, and drift reports it every day to a lane that cannot close it.
// For each such task: its open inbox request and its open task_flag rows are
// superseded, its open dots operator questions are closed, its block is
// cleared, and its request is withdrawn with withdrawn_reason task_settled.
// The statements run in that order, since the matches read the open refs,
// which the last statement changes.
//
// Idempotent: a second run matches no row, so it changes nothing.
//
// Expand-only: it changes values and adds one JSON key (withdrawn_reason),
// which the previous release does not read. It writes only status values the
// previous release already handles (withdrawn, superseded). Locks: ROW
// EXCLUSIVE on decision_requests, dots_results and tasks, and row locks on the
// matched rows only. Each statement scans its table without an index on refs,
// so the scan holds no lock on rows that do not match.
func init() {
	registerMigration(migration{ID: "20261125-settled-open-decision-withdraw", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`UPDATE decision_requests d SET status='superseded',superseded_by='withdrawn',updated_at=now()
				FROM tasks t
				WHERE d.source IN ('task_request','linear') AND d.status='open'
				  AND d.source_ref = t.refs->'decision_request'->>'id'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND t.refs->'decision_request'->>'status' = 'open'`,
			`UPDATE decision_requests d SET status='superseded',superseded_by='task:'||t.id||':'||t.state,updated_at=now()
				FROM tasks t
				WHERE d.task_id = t.id AND d.source='task_flag' AND d.status='open'
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND t.refs->'decision_request'->>'status' = 'open'`,
			`UPDATE dots_results d SET answered_at=now(),answered_by='task_settled'
				FROM tasks t
				WHERE d.task_id = t.id AND d.kind='QUESTION' AND d.ask_operator AND d.answered_at IS NULL
				  AND t.state IN ('merged','deployed','completed','dropped','duplicate')
				  AND t.refs->'decision_request'->>'status' = 'open'`,
			`UPDATE tasks SET awaiting_decision=false,
				refs = jsonb_set(refs, '{decision_request}', (refs->'decision_request') || '{"status":"withdrawn","withdrawn_reason":"task_settled"}'::jsonb)
				WHERE state IN ('merged','deployed','completed','dropped','duplicate')
				  AND refs->'decision_request'->>'status' = 'open'`,
		})
	}})
}

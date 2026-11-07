package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const operatorInboxMigrationID = "20261107-operator-inbox-state"

func init() {
	registerMigration(migration{ID: operatorInboxMigrationID, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, operatorInboxStatements)
	}})
}

// The operator's inbox triage needs two new tables, so this migration locks
// nothing that exists yet: it takes no table lock on a live table and cannot
// wait on a row a writer holds. operator_inbox_state keeps one row per
// attention item the operator has read, completed or snoozed (hk 3343).
// operator_inbox_events is the idempotency and undo ledger: it holds the state
// a write replaced together with the state it wrote, so a replayed event_key
// changes nothing, and an undo restores only the items no later write moved.
var operatorInboxStatements = []string{
	// item_key holds a deploy service name of up to 128 bytes plus the id.
	`CREATE TABLE operator_inbox_state (
 item_key TEXT PRIMARY KEY CHECK (length(item_key) <= 192),
 read_at TIMESTAMPTZ,
 done_at TIMESTAMPTZ,
 snoozed_until TIMESTAMPTZ,
 actor TEXT NOT NULL CHECK (length(actor) <= 320),
 updated_at TIMESTAMPTZ NOT NULL)`,
	`CREATE TABLE operator_inbox_events (
 event_key TEXT NOT NULL CHECK (length(event_key) <= 128),
 item_key TEXT NOT NULL CHECK (length(item_key) <= 192),
 action TEXT NOT NULL CHECK (action IN ('read','unread','done','undone','snooze')),
 previous_read_at TIMESTAMPTZ,
 previous_done_at TIMESTAMPTZ,
 previous_snoozed_until TIMESTAMPTZ,
 previous_request_id BIGINT,
 previous_request_snoozed_until TIMESTAMPTZ,
 applied_read_at TIMESTAMPTZ,
 applied_done_at TIMESTAMPTZ,
 applied_snoozed_until TIMESTAMPTZ,
 undone_at TIMESTAMPTZ,
 actor TEXT NOT NULL CHECK (length(actor) <= 320),
 at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (event_key, item_key))`,
}

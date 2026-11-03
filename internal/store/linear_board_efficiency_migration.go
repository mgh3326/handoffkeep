package store

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
)

// Version 32 mirrors metadata locally and collapses the old board backlog.
// CREATE UNIQUE INDEX and duplicate quarantine lock linear_issues; UPDATE locks
// superseded pending board rows. The new reservation FK takes SHARE ROW
// EXCLUSIVE on tasks. Other outbox kinds and sent history stay intact.
var linearBoardEfficiencyV32 = []string{
	`CREATE TABLE linear_metadata_state(singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),initialized BOOLEAN NOT NULL DEFAULT false)`,
	`INSERT INTO linear_metadata_state(singleton) VALUES(true)`,
	`CREATE TABLE linear_metadata(kind TEXT NOT NULL CHECK(kind IN ('team','state','label')),id TEXT NOT NULL,team_id TEXT NOT NULL,name TEXT NOT NULL,updated_at TIMESTAMPTZ NOT NULL,deleted BOOLEAN NOT NULL DEFAULT false,PRIMARY KEY(kind,id))`,
	`CREATE INDEX linear_metadata_lookup ON linear_metadata(kind,team_id,name) WHERE NOT deleted`,
	`CREATE TABLE linear_label_attempts(team_id TEXT NOT NULL,name TEXT NOT NULL,token TEXT NOT NULL,started_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(team_id,name))`,
	`CREATE TABLE linear_label_failures(team_id TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(team_id,name))`,
	`CREATE TABLE linear_issue_link_conflicts(snapshot JSONB NOT NULL,quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	// Preserve every conflicting row before removing it from the live reverse
	// lookup. A registry-owned pair wins, then latest update/create, then task id.
	`WITH ranked AS (SELECT l.ctid AS row_id,to_jsonb(l) AS snapshot,
 row_number() OVER(PARTITION BY l.task_id ORDER BY (o.task_id IS NOT NULL) DESC,l.updated_at DESC,l.created_at DESC,l.issue_id,l.identifier) AS task_rank,
 row_number() OVER(PARTITION BY l.issue_id ORDER BY (o.task_id IS NOT NULL) DESC,l.updated_at DESC,l.created_at DESC,l.task_id) AS issue_rank,
 row_number() OVER(PARTITION BY l.identifier ORDER BY (o.task_id IS NOT NULL) DESC,l.updated_at DESC,l.created_at DESC,l.task_id) AS identifier_rank
 FROM linear_issues l LEFT JOIN linear_board_owners o ON o.task_id=l.task_id AND o.issue_id=l.issue_id),
 removed AS (DELETE FROM linear_issues l USING ranked r WHERE l.ctid=r.row_id AND (r.task_rank>1 OR r.issue_rank>1 OR r.identifier_rank>1) RETURNING to_jsonb(l) AS snapshot)
 INSERT INTO linear_issue_link_conflicts(snapshot) SELECT snapshot FROM removed`,
	`CREATE TABLE linear_issue_webhook_versions(issue_id TEXT PRIMARY KEY,event_at TIMESTAMPTZ NOT NULL)`,
	`DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='linear_issues'::regclass AND contype='p') THEN ALTER TABLE linear_issues ADD PRIMARY KEY(task_id); END IF; END $$`,
	`CREATE UNIQUE INDEX linear_issues_issue_id ON linear_issues(issue_id)`,
	`CREATE UNIQUE INDEX linear_issues_identifier ON linear_issues(identifier)`,
	`WITH ranked AS (SELECT id,row_number() OVER(PARTITION BY task_id ORDER BY seq DESC,id DESC) AS position FROM linear_outbox WHERE op='work_board' AND state='pending')
 UPDATE linear_outbox SET state='skipped',last_error='superseded by latest pending board snapshot (migration 32)',updated_at=now() WHERE id IN (SELECT id FROM ranked WHERE position>1)`,
	`ALTER TABLE linear_create_admissions ADD COLUMN board_task_id BIGINT REFERENCES tasks(id),ADD COLUMN awaiting_webhook BOOLEAN NOT NULL DEFAULT false`,
	`CREATE TABLE linear_board_stopped_tasks(task_id BIGINT PRIMARY KEY REFERENCES tasks(id))`,
	`UPDATE linear_outbox SET attempts=0,next_attempt_at=now(),last_error='' WHERE op='work_board' AND state='pending'`,
	`INSERT INTO schema_version(version) VALUES(32)`,
}

// Match old stable board ids in Go; no database crypto extension or title
// inference is required. Unknown non-board reservations stay recoverable by id.
func backfillLinearBoardReservations(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT issue_id FROM linear_create_admissions WHERE board_task_id IS NULL`)
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pending[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(pending) == 0 {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		return err
	}
	matches := map[string]int64{}
	for rows.Next() {
		var taskID int64
		if err = rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		id := LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", taskID))
		if pending[id] {
			matches[id] = taskID
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for id, taskID := range matches {
		if _, err = tx.Exec(ctx, `UPDATE linear_create_admissions SET board_task_id=$2 WHERE issue_id=$1 AND board_task_id IS NULL`, id, taskID); err != nil {
			return err
		}
	}
	return nil
}

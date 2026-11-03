package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strconv"
	"time"
)

var boardTaskMarker = regexp.MustCompile(`<!-- work-board:task:([1-9][0-9]*) -->`)
var boardIdentifier = regexp.MustCompile(`^ROB-[1-9][0-9]*$`)

type LinearBoardIssueEvent struct {
	IssueID, Identifier, Description, TeamID string
	Inactive                                 bool
	At                                       time.Time
}

// Only authenticated Issue webhooks call this path. Outbound delivery never
// populates linear_issues. Missing list entries are never interpreted as deletion.
func (s *Store) RecordLinearBoardIssue(ctx context.Context, event LinearBoardIssueEvent) error {
	if event.IssueID == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180176)`); err != nil {
		return err
	}
	var taskID int64
	err = tx.QueryRow(ctx, `SELECT task_id FROM linear_issues WHERE issue_id=$1 UNION SELECT task_id FROM linear_board_owners WHERE issue_id=$1 UNION SELECT board_task_id FROM linear_create_admissions WHERE issue_id=$1 AND board_task_id IS NOT NULL LIMIT 1`, event.IssueID).Scan(&taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if taskID == 0 {
		marker := boardTaskMarker.FindStringSubmatch(event.Description)
		if len(marker) != 2 {
			return nil
		}
		taskID, err = strconv.ParseInt(marker[1], 10, 64)
		if err != nil {
			return nil
		}
		// A new mapping needs the deterministic identity and a committed hk create
		// reservation. A marker copied into somebody else's issue cannot take it.
		if event.IssueID != LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", taskID)) {
			return nil
		}
		var reserved bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_create_admissions WHERE issue_id=$1 AND (board_task_id=$2 OR board_task_id IS NULL))`, event.IssueID, taskID).Scan(&reserved); err != nil {
			return err
		}
		if !reserved {
			return nil
		}
	}
	if event.At.IsZero() {
		return errors.New("missing linear webhook event time")
	}
	var currentAt time.Time
	err = tx.QueryRow(ctx, `SELECT event_at FROM linear_issue_webhook_versions WHERE issue_id=$1`, event.IssueID).Scan(&currentAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !currentAt.Before(event.At) {
		return nil
	}
	if event.Inactive {
		if _, err = tx.Exec(ctx, `INSERT INTO linear_board_stopped_tasks(task_id) VALUES($1) ON CONFLICT DO NOTHING`, taskID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs,'{linear}',COALESCE(NULLIF(refs->'linear','null'::jsonb),'{}'::jsonb)-'issue_id'-'identifier') WHERE id=$1`, taskID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM linear_issues WHERE issue_id=$1`, event.IssueID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM linear_board_owners WHERE issue_id=$1`, event.IssueID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM linear_create_admissions WHERE issue_id=$1`, event.IssueID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO linear_issue_webhook_versions(issue_id,event_at) VALUES($1,$2) ON CONFLICT(issue_id) DO UPDATE SET event_at=EXCLUDED.event_at`, event.IssueID, event.At); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var stopped bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_board_stopped_tasks WHERE task_id=$1)`, taskID).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return nil
	}
	// Ignore incomplete payloads; never do an outbound search from a webhook.
	if !boardIdentifier.MatchString(event.Identifier) {
		return nil
	}
	if event.TeamID != "" {
		var teamOK bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_metadata WHERE kind='team' AND id=$1 AND name='ROB' AND NOT deleted)`, event.TeamID).Scan(&teamOK); err != nil {
			return err
		}
		if !teamOK {
			return nil
		}
	}
	if err = reserveLinearBoardOwnershipTx(ctx, tx, taskID, event.IssueID, event.Identifier); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO linear_issues(task_id,issue_id,identifier,created_at,updated_at) VALUES($1,$2,$3,now(),now())
 ON CONFLICT(task_id) DO UPDATE SET identifier=EXCLUDED.identifier,updated_at=EXCLUDED.updated_at WHERE linear_issues.issue_id=EXCLUDED.issue_id`, taskID, event.IssueID, event.Identifier); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs,'{linear}',COALESCE(NULLIF(refs->'linear','null'::jsonb),'{}'::jsonb)||jsonb_build_object('issue_id',$2::text,'identifier',$3::text)) WHERE id=$1`, taskID, event.IssueID, event.Identifier); err != nil {
		return err
	}
	// Wake waiting deliveries without inverting the outbox -> ownership lock
	// order. A currently locked row finishes its own short pending retry.
	if _, err = tx.Exec(ctx, `UPDATE linear_outbox SET next_attempt_at=now(),last_error='' WHERE id IN
    (SELECT id FROM linear_outbox WHERE task_id=$1 AND op='work_board' AND state='pending' ORDER BY id FOR UPDATE SKIP LOCKED)`, taskID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM linear_create_admissions WHERE issue_id=$1`, event.IssueID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO linear_issue_webhook_versions(issue_id,event_at) VALUES($1,$2) ON CONFLICT(issue_id) DO UPDATE SET event_at=EXCLUDED.event_at`, event.IssueID, event.At); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

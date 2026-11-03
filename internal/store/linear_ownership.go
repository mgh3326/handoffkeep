package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrLinearIssueOwnership = errors.New("linear_issue_ownership_conflict")

func (s *Store) GetLinearBoard(ctx context.Context, id int64) (LinearBoard, bool, error) {
	var b LinearBoard
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT status FROM linear_board_tasks WHERE task_id=$1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return b, false, err
	}
	return b, true, nil
}

// Reserve ownership before any issue mutation, including reuse by ROB title.
// The separate committed reservation survives a failed write/readback. Legacy
// duplicate links are refused, never silently reassigned or removed.
func (s *Store) EnsureLinearBoardOwnership(ctx context.Context, taskID int64, issueID, identifier string) error {
	if taskID < 1 || issueID == "" || identifier == "" {
		return ErrLinearIssueOwnership
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = reserveLinearBoardOwnershipTx(ctx, tx, taskID, issueID, identifier); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) HasLinearCreateReservation(ctx context.Context, id string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_create_admissions WHERE issue_id=$1)`, id).Scan(&pending)
	return pending, err
}

func reserveLinearBoardOwnershipTx(ctx context.Context, tx pgx.Tx, taskID int64, issueID, identifier string) error {
	var err error
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180176)`); err != nil {
		return err
	}
	var conflict bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_issues WHERE (issue_id=$2 AND task_id<>$1) OR (task_id=$1 AND issue_id<>$2) OR (identifier=$3 AND (task_id<>$1 OR issue_id<>$2)))
 OR EXISTS(SELECT 1 FROM linear_decision_issues WHERE issue_id=$2)
 OR EXISTS(SELECT 1 FROM linear_board_owners WHERE (issue_id=$2 AND task_id<>$1) OR (task_id=$1 AND issue_id<>$2))`, taskID, issueID, identifier).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return ErrLinearIssueOwnership
	}
	if _, err = tx.Exec(ctx, `INSERT INTO linear_board_owners(issue_id,task_id) VALUES($2,$1) ON CONFLICT DO NOTHING`, taskID, issueID); err != nil {
		return err
	}
	// Outbound work reserves ownership only. The signed Issue webhook is the
	// sole writer of the active task/issue mapping and reservation completion.
	return nil
}
func (s *Store) LinearBoardDeliveryStopped(ctx context.Context, taskID int64) (bool, error) {
	var stopped bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM linear_board_stopped_tasks WHERE task_id=$1)`, taskID).Scan(&stopped)
	return stopped, err
}

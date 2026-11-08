package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// YouTrackDecisionMirror links one decision request to its YouTrack DEC issue.
// Stamp is the request's updated_at at the last write; an empty stamp means
// the row was adopted and the next sync rewrites the issue once.
type YouTrackDecisionMirror struct {
	DecisionID int64  `json:"decision_id"`
	IssueID    string `json:"issue_id"`
	DisplayRef string `json:"display_ref,omitempty"`
	Stamp      string `json:"stamp"`
}

// GetYouTrackDecisionMirror returns the mirror row of one decision, if any.
func (s *Store) GetYouTrackDecisionMirror(ctx context.Context, decisionID int64) (YouTrackDecisionMirror, bool, error) {
	var x YouTrackDecisionMirror
	err := s.pool.QueryRow(ctx, `SELECT decision_id,issue_id,display_ref,stamp FROM youtrack_decision_mirrors WHERE decision_id=$1`, decisionID).
		Scan(&x.DecisionID, &x.IssueID, &x.DisplayRef, &x.Stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackDecisionMirror{}, false, nil
	}
	return x, err == nil, err
}

// ListYouTrackDecisionMirrors returns the mirror rows for the given decisions,
// keyed by decision id.
func (s *Store) ListYouTrackDecisionMirrors(ctx context.Context, decisionIDs []int64) (map[int64]YouTrackDecisionMirror, error) {
	out := map[int64]YouTrackDecisionMirror{}
	if len(decisionIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT decision_id,issue_id,display_ref,stamp FROM youtrack_decision_mirrors WHERE decision_id=ANY($1)`, decisionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x YouTrackDecisionMirror
		if err := rows.Scan(&x.DecisionID, &x.IssueID, &x.DisplayRef, &x.Stamp); err != nil {
			return nil, err
		}
		out[x.DecisionID] = x
	}
	return out, rows.Err()
}

// PutYouTrackDecisionMirror records the issue a decision maps to. It replaces
// the row for the same decision.
func (s *Store) PutYouTrackDecisionMirror(ctx context.Context, x YouTrackDecisionMirror) error {
	if x.DecisionID < 1 || strings.TrimSpace(x.IssueID) == "" {
		return ErrYouTrackMirrorConflict
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtrack_decision_mirrors(decision_id,issue_id,display_ref,stamp,created_at)
		VALUES($1,$2,$3,$4,now())
		ON CONFLICT(decision_id) DO UPDATE SET issue_id=EXCLUDED.issue_id, display_ref=EXCLUDED.display_ref, stamp=EXCLUDED.stamp`,
		x.DecisionID, x.IssueID, x.DisplayRef, x.Stamp)
	return err
}

package store

import (
	"context"
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

// ListYouTrackDecisionMirrors returns the mirror rows for the given decisions,
// keyed by decision id. Its one reader is the decisions backfill.
func (s *Store) ListYouTrackDecisionMirrors(ctx context.Context, decisionIDs []int64) (map[int64]YouTrackDecisionMirror, error) {
	if s != nil && s.dialect == mysqlDialect {
		// Not ported to MySQL: youtrack_decision_mirrors is left out of MySQL (design §2) (hk 5088).
		return nil, ErrDialectUnsupported
	}

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

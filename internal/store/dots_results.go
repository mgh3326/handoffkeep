package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// A dots_results row is written by RecordDotsResult for each recorded dots
// comment (schema: migration_dots_results.go). Its adoption state says what
// became of the answer; it starts at pending_review and is set by a person or
// a session reviewing the answer (design v1 §12.3, §13.1 R13).

// Adoption states.
const (
	DotsAdoptionPending    = "pending_review"
	DotsAdoptionAccepted   = "accepted"
	DotsAdoptionPartial    = "partially_used"
	DotsAdoptionRejected   = "rejected"
	DotsAdoptionSuperseded = "superseded"
)

// DotsResultListMax is the most rows ListDotsResults returns.
const DotsResultListMax = 500

var (
	// ErrDotsResultNotFound is a dots_results id that names no row.
	ErrDotsResultNotFound = errors.New("dots_result_not_found")
	// ErrDotsAdoptionFinal refuses a change to a superseded result.
	ErrDotsAdoptionFinal = errors.New("dots_adoption_final")
)

var (
	dotsPRRefRE   = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[1-9][0-9]*$`)
	dotsTaskRefRE = regexp.MustCompile(`^hk:task/[1-9][0-9]*$`)
	dotsDocRefRE  = regexp.MustCompile(`^hk:doc (\S+)$`)
)

// DotsResult is one dots_results row. Unknown values are absent.
type DotsResult struct {
	ID              int64      `json:"id"`
	DocumentKey     string     `json:"document_key"`
	CommentID       string     `json:"comment_id"`
	IssueUUID       string     `json:"issue_uuid"`
	IssueIdentifier string     `json:"issue_identifier"`
	Kind            string     `json:"kind"`
	Verdict         string     `json:"verdict,omitempty"`
	ArtefactSHA256  string     `json:"artefact_sha256"`
	TaskID          *int64     `json:"task_id,omitempty"`
	RoutedLane      string     `json:"routed_lane"`
	AdoptionState   string     `json:"adoption_state"`
	AdoptedRef      string     `json:"adopted_ref,omitempty"`
	AdoptedBy       string     `json:"adopted_by,omitempty"`
	AdoptedAt       *time.Time `json:"adopted_at,omitempty"`
	SupersededBy    *int64     `json:"superseded_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// DotsAdoptionInput is the body of POST /v1/dots/results/{id}/adoption.
type DotsAdoptionInput struct {
	State        string `json:"state"`
	Ref          string `json:"ref,omitempty"`
	SupersededBy int64  `json:"superseded_by,omitempty"`
}

const dotsResultColumns = `id,document_key,comment_id,issue_uuid,issue_identifier,kind,COALESCE(verdict,''),artefact_sha256,task_id,routed_lane,adoption_state,COALESCE(adopted_ref,''),COALESCE(adopted_by,''),adopted_at,superseded_by,created_at,updated_at`

func scanDotsResult(row pgx.Row, x *DotsResult) error {
	return row.Scan(&x.ID, &x.DocumentKey, &x.CommentID, &x.IssueUUID, &x.IssueIdentifier, &x.Kind, &x.Verdict, &x.ArtefactSHA256, &x.TaskID, &x.RoutedLane, &x.AdoptionState, &x.AdoptedRef, &x.AdoptedBy, &x.AdoptedAt, &x.SupersededBy, &x.CreatedAt, &x.UpdatedAt)
}

// ListDotsResults lists the results of one task (taskID > 0) or of one Linear
// issue by identifier, newest first.
func (s *Store) ListDotsResults(ctx context.Context, taskID int64, issue string, limit int) ([]DotsResult, error) {
	if (taskID > 0) == (issue != "") || taskID < 0 || (issue != "" && !dotsIdentifierRE.MatchString(issue)) {
		return nil, errors.New("invalid dots results query: give a task id or an issue identifier")
	}
	if limit < 1 {
		limit = 100
	}
	limit = min(limit, DotsResultListMax)
	where, arg := `task_id=$1`, any(taskID)
	if issue != "" {
		where, arg = `issue_identifier=$1`, issue
	}
	rows, err := s.pool.Query(ctx, `SELECT `+dotsResultColumns+` FROM dots_results WHERE `+where+` ORDER BY id DESC LIMIT $2`, arg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DotsResult{}
	for rows.Next() {
		var x DotsResult
		if err := scanDotsResult(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// checkDotsAdoption refuses a malformed adoption change. accepted and
// partially_used name what adopted the answer; superseded names the result
// that replaced it, and only superseded does.
func checkDotsAdoption(id int64, in DotsAdoptionInput) error {
	switch in.State {
	case DotsAdoptionAccepted, DotsAdoptionPartial, DotsAdoptionRejected, DotsAdoptionSuperseded:
	default:
		return errors.New("invalid dots adoption: state is accepted, partially_used, rejected or superseded")
	}
	if in.Ref == "" && (in.State == DotsAdoptionAccepted || in.State == DotsAdoptionPartial) {
		return errors.New("invalid dots adoption: " + in.State + " needs a ref")
	}
	if in.Ref != "" && !validDotsAdoptedRef(in.Ref) {
		return errors.New("invalid dots adoption: ref is a GitHub PR URL, hk:task/<id> or hk:doc <key>")
	}
	if (in.State == DotsAdoptionSuperseded) != (in.SupersededBy != 0) {
		return errors.New("invalid dots adoption: superseded_by is required with superseded and only with it")
	}
	if in.SupersededBy < 0 || in.SupersededBy == id {
		return errors.New("invalid dots adoption: superseded_by names another result")
	}
	return nil
}

func validDotsAdoptedRef(ref string) bool {
	if dotsPRRefRE.MatchString(ref) || dotsTaskRefRE.MatchString(ref) {
		return true
	}
	m := dotsDocRefRE.FindStringSubmatch(ref)
	return m != nil && validDocKey(m[1])
}

// SetDotsAdoption moves a result to an adoption state. Repeating the stored
// change changes nothing and answers changed=false; superseded is final, so
// any other change to a superseded result is ErrDotsAdoptionFinal. It locks
// one dots_results row and reads the superseding one.
func (s *Store) SetDotsAdoption(ctx context.Context, id int64, by string, in DotsAdoptionInput) (DotsResult, bool, error) {
	if id < 1 || by == "" || !validText(by, 128) {
		return DotsResult{}, false, errors.New("invalid dots adoption")
	}
	if err := checkDotsAdoption(id, in); err != nil {
		return DotsResult{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DotsResult{}, false, err
	}
	defer tx.Rollback(ctx)
	var cur DotsResult
	if err := scanDotsResult(tx.QueryRow(ctx, `SELECT `+dotsResultColumns+` FROM dots_results WHERE id=$1 FOR UPDATE`, id), &cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DotsResult{}, false, ErrDotsResultNotFound
		}
		return DotsResult{}, false, err
	}
	var curSupersededBy int64
	if cur.SupersededBy != nil {
		curSupersededBy = *cur.SupersededBy
	}
	if cur.AdoptionState == in.State && cur.AdoptedRef == in.Ref && curSupersededBy == in.SupersededBy {
		return cur, false, nil
	}
	if cur.AdoptionState == DotsAdoptionSuperseded {
		return DotsResult{}, false, ErrDotsAdoptionFinal
	}
	var supersededBy *int64
	if in.SupersededBy != 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dots_results WHERE id=$1)`, in.SupersededBy).Scan(&exists); err != nil {
			return DotsResult{}, false, err
		}
		if !exists {
			return DotsResult{}, false, errors.New("invalid dots adoption: superseded_by names no result")
		}
		supersededBy = &in.SupersededBy
	}
	var out DotsResult
	err = scanDotsResult(tx.QueryRow(ctx, `UPDATE dots_results SET adoption_state=$2, adopted_ref=$3, adopted_by=$4, adopted_at=$5, superseded_by=$6, updated_at=$5 WHERE id=$1 RETURNING `+dotsResultColumns,
		id, in.State, nullIfEmpty(in.Ref), by, time.Now().UTC(), supersededBy), &out)
	if err != nil {
		return DotsResult{}, false, err
	}
	return out, true, tx.Commit(ctx)
}

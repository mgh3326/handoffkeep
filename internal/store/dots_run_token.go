package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// A run token is the secret START returns to the dots conversation that
// started a request. Every later reply on that run must carry it, so two
// conversations that share the operator principal cannot write into each
// other's run.
//
// The token is stored as given, not hashed. A replayed START must return its
// first result, token included, and that needs the plain value. The token
// only tells the runs of one request apart; it is not a user credential, and
// anyone who can read dots_run_tokens already reads the whole database.

var (
	// ErrDotsRunTokenRequired refuses a reply that names no run token. START
	// is the only reply that does not need one.
	ErrDotsRunTokenRequired = errors.New("dots_run_token_required")
	// ErrDotsRunTokenMismatch refuses a reply whose run token is not the
	// request's active token, including a malformed one and a request that
	// has never been started.
	ErrDotsRunTokenMismatch = errors.New("dots_run_token_mismatch")
)

var dotsRunTokenRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// newDotsRunToken returns 256 random bits as 64 lower-case hex characters.
func newDotsRunToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validDotsRunToken reports whether a presented token has the issued shape.
// A malformed token cannot match, so it is refused before any state is read.
func validDotsRunToken(token string) bool {
	return dotsRunTokenRE.MatchString(token)
}

// checkDotsRunTokenTx refuses unless token is the request's active run token.
// The caller holds the task row lock, so the active token cannot change
// during the check.
func checkDotsRunTokenTx(ctx context.Context, tx pgx.Tx, taskID int64, token string) error {
	if !validDotsRunToken(token) {
		return ErrDotsRunTokenMismatch
	}
	var stored string
	err := tx.QueryRow(ctx, `SELECT token FROM dots_run_tokens WHERE task_id=$1 AND superseded_at IS NULL`, taskID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDotsRunTokenMismatch
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(token)) != 1 {
		return ErrDotsRunTokenMismatch
	}
	return nil
}

// issueDotsRunTokenTx makes a new token the request's only active one and
// returns it. issueKey is the reply key of the START that issued it, which
// is what a replay of that START looks up. A takeover supersedes the active
// token and records the handover in the request's history, naming the keys
// of the superseded and the new token. The history holds no token value.
func issueDotsRunTokenTx(ctx context.Context, tx pgx.Tx, taskID int64, issueKey, actor string, takeover bool, now time.Time) (string, error) {
	token, err := newDotsRunToken()
	if err != nil {
		return "", err
	}
	var previous string
	if takeover {
		err := tx.QueryRow(ctx, `UPDATE dots_run_tokens SET superseded_at=$2 WHERE task_id=$1 AND superseded_at IS NULL RETURNING issue_key`, taskID, now).Scan(&previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dots_run_tokens(task_id,token,issue_key,takeover,issued_by,issued_at) VALUES($1,$2,$3,$4,$5,$6)`, taskID, token, issueKey, takeover, actor, now); err != nil {
		return "", err
	}
	if takeover {
		if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: taskID, Kind: "source_event", By: actor, Note: "takeover", At: now, Source: "dots-run-token", SourceRef: issueKey, Field: "run_token", FromValue: previous, ToValue: issueKey}); err != nil {
			return "", err
		}
	}
	return token, nil
}

// dotsRunTokenForIssueTx returns the token the START (or takeover) with this
// reply key issued, for a replay. It is empty when that START issued none,
// which only a request started before tokens existed can have. The token may
// since have been superseded; a later reply carrying it is refused.
func dotsRunTokenForIssueTx(ctx context.Context, tx pgx.Tx, taskID int64, issueKey string) (string, error) {
	var token string
	err := tx.QueryRow(ctx, `SELECT token FROM dots_run_tokens WHERE task_id=$1 AND issue_key=$2`, taskID, issueKey).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return token, err
}

// DotsRunIssuedAt returns when the request's active run token was issued, or
// nil when it has none. The inbox reports this time; it never reports the
// token.
func (s *Store) DotsRunIssuedAt(ctx context.Context, taskID int64) (*time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT issued_at FROM dots_run_tokens WHERE task_id=$1 AND superseded_at IS NULL`, taskID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

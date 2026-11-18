package store

import (
	"context"
	"errors"
	"time"
)

// dots_inbox_reads holds one row per dots_inbox MCP call (hk 4924): the server
// time, the authenticated principal (never a token), how many requests the
// call listed and its result — "ok" or the error the caller saw. dots checks
// its inbox on a schedule the operator cannot see from hk; these rows make
// the check visible (`handoffkeep dots reads`).

// DotsInboxReadRetention bounds the table: RecordDotsInboxRead deletes every
// row older than this in the same transaction as the insert, so the table
// never outgrows the window (about 4,400 rows at one read per ten minutes).
const DotsInboxReadRetention = 30 * 24 * time.Hour

// DotsInboxReadListMax is the most rows ListDotsInboxReads returns.
const DotsInboxReadListMax = 500

// DotsInboxRead is one recorded dots_inbox call.
type DotsInboxRead struct {
	ID        int64     `json:"id"`
	ReadAt    time.Time `json:"read_at"`
	Principal string    `json:"principal"`
	Listed    int       `json:"listed"`
	Result    string    `json:"result"`
}

const dotsInboxReadColumns = `id,read_at,principal,listed,result`

// RecordDotsInboxRead appends one read row and prunes rows older than
// DotsInboxReadRetention in the same transaction. The time is the caller's
// clock (the server's), never the database's.
func (s *Store) RecordDotsInboxRead(ctx context.Context, principal string, listed int, result string) error {
	if s == nil || principal == "" || !validText(principal, 320) || listed < 0 || listed > 1<<20 || result == "" || !validText(result, 512) {
		return errors.New("invalid dots inbox read")
	}
	now := time.Now().UTC()
	cutoff := now.Add(-DotsInboxReadRetention)
	return s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO dots_inbox_reads(principal,listed,result,read_at) VALUES($1,$2,$3,$4)`, `INSERT INTO dots_inbox_reads(principal,listed,result,read_at) VALUES(?,?,?,?)`), principal, listed, result, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sqlFor(tx, `DELETE FROM dots_inbox_reads WHERE read_at < $1`, `DELETE FROM dots_inbox_reads WHERE read_at < ?`), cutoff); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// ListDotsInboxReads lists recorded reads newest first. A zero since lists
// every retained read; a set since keeps reads at or after it.
func (s *Store) ListDotsInboxReads(ctx context.Context, since time.Time, limit int) ([]DotsInboxRead, error) {
	if limit < 1 || limit > DotsInboxReadListMax {
		return nil, errors.New("invalid dots inbox reads limit")
	}
	sel := `SELECT ` + dotsInboxReadColumns + ` FROM dots_inbox_reads`
	order := ` ORDER BY read_at DESC, id DESC`
	var query string
	var args []any
	if since.IsZero() {
		query = s.q(sel+order+` LIMIT $1`, sel+order+` LIMIT ?`)
		args = []any{limit}
	} else {
		query = s.q(sel+` WHERE read_at >= $1`+order+` LIMIT $2`, sel+` WHERE read_at >= ?`+order+` LIMIT ?`)
		args = []any{since.UTC(), limit}
	}
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DotsInboxRead{}
	for rows.Next() {
		var x DotsInboxRead
		if err := rows.Scan(&x.ID, &x.ReadAt, &x.Principal, &x.Listed, &x.Result); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// Family roots (hk 4797, yt_family_roots): the article_create kind records,
// per family and project, the root article every document of the family goes
// under, and takes the family's lane while it finds or creates one. Two
// documents of a new family cannot make two roots: the second finds the lane
// taken, leaves its op for a later pass, and then reads the root the first
// recorded.
//
// Lock order (lockorder.go): the family lane is a session advisory lock on a
// connection of its own, held outside any transaction, never while the ledger
// holds a transaction's lock or a row. It is taken before the article_create
// send's remote calls and released before Send returns; a process that dies
// drops its connection and with it the lock.

// ytFamilyUnlockTimeout bounds the release of a family lane: the release must
// run even when the dispatch's own context is done, and must not hang a
// stopping worker.
const ytFamilyUnlockTimeout = 5 * time.Second

// ytFamilyLaneKey is the advisory lock key of one family's root: the tenant,
// project and family, which is the identity of a root (yt_family_roots).
func ytFamilyLaneKey(tenant, projectID, family string) string {
	return "yt_family_root:" + tenant + "\x00" + projectID + "\x00" + family
}

// LockFamily takes the family's lane without waiting (hk 4797). ok is false
// when another session holds it: the caller leaves its op for a later pass
// instead of blocking a worker's pass behind one slow create. The lock is a
// session lock on the connection held until unlock, so it spans the remote
// calls the root's search and create make.
func (l *YTWriteLedger) LockFamily(ctx context.Context, tenant, projectID, family string) (unlock func(), ok bool, err error) {
	if l.s == nil || l.s.pool == nil {
		return nil, false, errors.New("store: no database pool for the family lane")
	}
	key := ytFamilyLaneKey(tenant, projectID, family)
	conn, err := l.s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), ytFamilyUnlockTimeout)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			// A connection that returns to the pool still holding the lock
			// would keep the family's lane closed for its idle life: drop the
			// session instead, which releases the lock with it.
			log.Printf("ytwrite.family_unlock_failed family=%q project_id=%q err=%q", family, projectID, err.Error())
			hijacked := conn.Hijack()
			_ = hijacked.Close(context.Background())
			return
		}
		conn.Release()
	}, true, nil
}

// FamilyRoot is the root article the store recorded for a family on one
// project; found is false when none is recorded.
func (l *YTWriteLedger) FamilyRoot(ctx context.Context, tenant, projectID, family string) (string, bool, error) {
	var id string
	err := l.s.pool.QueryRow(ctx, `SELECT article_id FROM yt_family_roots WHERE tenant=$1 AND project_id=$2 AND family=$3`,
		tenant, projectID, family).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// RecordFamilyRoot records the root a document's send resolved for its family,
// so the family's later documents need no search (hk 4797). The first record
// wins: a root is never replaced, because the one already recorded is the one
// the family's articles are under.
func (l *YTWriteLedger) RecordFamilyRoot(ctx context.Context, tenant, projectID, family, articleID string, opID int64) error {
	_, err := l.s.pool.Exec(ctx, `INSERT INTO yt_family_roots(tenant, project_id, family, article_id, op_id)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (tenant, project_id, family) DO NOTHING`,
		tenant, projectID, family, articleID, opID)
	return err
}

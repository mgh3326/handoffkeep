package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// LinearArchiveLease adds watermark state, verified copies, and a final
// protection barrier to the existing serialized create lease.
type LinearArchiveLease interface {
	LinearCreateLease
	ArchiveCycle(context.Context, *bool) (bool, error)
	CopyArchive(context.Context, string, string) (string, error)
	ArchiveIfUnprotected(context.Context, string, string, time.Time, func() error) (bool, error)
}

func (l linearCreateLease) ArchiveCycle(ctx context.Context, set *bool) (bool, error) {
	if set != nil {
		if _, err := l.conn.Exec(ctx, `UPDATE linear_archive_state SET draining=$1 WHERE singleton`, *set); err != nil {
			return false, err
		}
	}
	var active bool
	err := l.conn.QueryRow(ctx, `SELECT draining FROM linear_archive_state WHERE singleton`).Scan(&active)
	return active, err
}

// CopyArchive is an immutable, content-addressed snapshot. The SQL statement
// commits before readback and before any archive attempt on the same lease.
func (l linearCreateLease) CopyArchive(ctx context.Context, issueID, body string) (string, error) {
	if !validName(issueID) || body == "" || !validText(body, DocumentMaxBytes) {
		return "", errors.New("invalid archive copy")
	}
	if err := guard.Reject(body); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(body))
	sum := hex.EncodeToString(digest[:])
	key := "linear/archive/" + issueID + "/" + sum
	if _, err := l.conn.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,'report','','',$2,$3,'linear-archive',now(),now()) ON CONFLICT(key) DO NOTHING`, key, body, sum); err != nil {
		return "", err
	}
	var storedBody, storedSHA string
	if err := l.conn.QueryRow(ctx, `SELECT body,sha256 FROM documents WHERE key=$1`, key).Scan(&storedBody, &storedSHA); err != nil {
		return "", err
	}
	if storedBody != body || storedSHA != sum {
		return "", errors.New("archive copy readback mismatch")
	}
	return key, nil
}

// This barrier shares the task/wrk writer lock. Local task/decision edits
// cannot introduce an open-work link between the final check and mutation.
func (l linearCreateLease) ArchiveIfUnprotected(ctx context.Context, id, identifier string, cutoff time.Time, mutate func() error) (bool, error) {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return false, err
	}
	protected, err := linearArchiveProtected(ctx, tx, id, identifier, cutoff)
	if err != nil || protected {
		return false, err
	}
	if err = mutate(); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func linearArchiveProtected(ctx context.Context, tx pgx.Tx, id, identifier string, cutoff time.Time) (bool, error) {
	var protected bool
	err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM tasks t LEFT JOIN documents d ON d.key=split_part(t.body_doc,'#',1)
 WHERE t.state NOT IN ('merged','dropped') AND (
 t.id IN (SELECT task_id FROM linear_issues WHERE issue_id=$1)
 OR strpos(t.refs::text,$1)>0 OR strpos(t.refs::text,$2)>0 OR strpos(t.title,$2)>0
 OR strpos(COALESCE(d.body,''),$1)>0 OR strpos(COALESCE(d.body,''),$2)>0))
 OR EXISTS(SELECT 1 FROM linear_decision_issues l JOIN tasks t ON t.id=l.task_id
 WHERE l.issue_id=$1 AND t.refs->'decision_request'->>'id'=l.request_id AND t.refs->'decision_request'->>'status'='open')
 OR EXISTS(SELECT 1 FROM task_events e WHERE e.at>$3 AND (strpos(COALESCE(e.refs::text,''),$1)>0 OR strpos(COALESCE(e.refs::text,''),$2)>0 OR strpos(e.note,$2)>0))
 OR EXISTS(SELECT 1 FROM tasks t WHERE t.updated_at>$3 AND (strpos(t.refs::text,$1)>0 OR strpos(t.refs::text,$2)>0 OR strpos(t.title,$2)>0))
 OR EXISTS(SELECT 1 FROM documents d WHERE d.updated_at>$3 AND strpos(d.key,'linear/archive/')<>1 AND (strpos(d.body,$1)>0 OR strpos(d.body,$2)>0))`, id, identifier, cutoff).Scan(&protected)
	if err != nil || protected {
		return protected, err
	}
	// Relation support may arrive after this migration. Once present, an
	// edge from open work protects either endpoint's issue too.
	var relations bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('task_relations') IS NOT NULL`).Scan(&relations); err != nil {
		return false, err
	}
	if relations {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_relations r JOIN tasks a ON a.id=r.source_id JOIN tasks b ON b.id=r.target_id
 WHERE r.active AND ((a.state NOT IN ('merged','dropped') AND (b.id IN (SELECT task_id FROM linear_issues WHERE issue_id=$1) OR b.refs->'linear'->>'issue_id'=$1 OR b.refs->'linear'->>'identifier'=$2))
 OR (b.state NOT IN ('merged','dropped') AND (a.id IN (SELECT task_id FROM linear_issues WHERE issue_id=$1) OR a.refs->'linear'->>'issue_id'=$1 OR a.refs->'linear'->>'identifier'=$2))))`, id, identifier).Scan(&protected)
	}
	return protected, err
}

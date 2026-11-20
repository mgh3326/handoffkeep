package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The article backfill (hk 5265, design hk5085 §Boundary and S13).
// `youtrack articles-backfill --apply` enqueues, through prepareYTArticleTx
// and the same gate the catch-up uses, the article op of a document the
// import did not bind: a short unbound one (its create), and a long one (its
// split). It creates nothing the protocol would not, and it never binds or
// updates a document: the binding of a verified imported article stays the
// command's own, and a bound document is refused here.

// YTArticleWaitingFamilyRoot is the named reason a long document shows while
// its family has no recorded root: a split creates none (S13), so it waits
// until a single-article create of the family makes one.
const YTArticleWaitingFamilyRoot = "waiting_family_root"

// ytArticleFamilyUncertain is the outcome of a document whose family has a
// root create left uncertain (remote_unknown, no root recorded): nothing of
// that family is enqueued until the create settles (hk 5265 item 3).
const ytArticleFamilyUncertain ytArticleOutcome = "family_root_uncertain"

// ErrYTArticleBackfillMode is refused when the document mode is off: the gate
// admits no article op then, so the backfill prepares none.
var ErrYTArticleBackfillMode = errors.New("store: HANDOFFKEEP_YT_DOCS_MODE is off: the article backfill prepares no op")

// YTArticleFamilyOf is a document key's family: its first segment when the key
// has a '/' and the segment names a family (ytwrite.ValidFamily), else "". It
// is ytArticleFamily without that function's log line, for the read-only
// callers.
func YTArticleFamilyOf(key string) string {
	first, _, found := strings.Cut(key, "/")
	if !found || !ytwrite.ValidFamily(first) {
		return ""
	}
	return first
}

// YTArticleFamilyStates is what the tenant's family roots look like: Recorded
// holds the families whose root the store recorded (yt_family_roots), and
// Uncertain the families with a root create left remote_unknown and not yet
// recorded (no request may be repeated for them, and nothing of them is
// enqueued).
type YTArticleFamilyStates struct {
	Recorded  map[string]bool
	Uncertain map[string]bool
}

// ytArticleFamilyStatesTx reads YTArticleFamilyStates on q (a read-only pool
// or a transaction).
func ytArticleFamilyStatesTx(ctx context.Context, q querier, tenant string) (YTArticleFamilyStates, error) {
	out := YTArticleFamilyStates{Recorded: map[string]bool{}, Uncertain: map[string]bool{}}
	rows, err := q.Query(ctx, sqlFor(q, `SELECT family FROM yt_family_roots WHERE tenant=$1`,
		`SELECT family FROM yt_family_roots WHERE tenant=?`), tenant)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			rows.Close()
			return out, err
		}
		out.Recorded[family] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ops, err := q.Query(ctx, sqlFor(q, `SELECT payload FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND tenant=$3 AND state='remote_unknown'`,
		`SELECT payload FROM yt_write_ops WHERE principal=? AND kind=? AND tenant=? AND state='remote_unknown'`),
		YTDocsPrincipal, ytwrite.KindArticleCreate, tenant)
	if err != nil {
		return out, err
	}
	defer ops.Close()
	for ops.Next() {
		var payload []byte
		if err := ops.Scan(&payload); err != nil {
			return out, err
		}
		var p ytwrite.ArticleCreatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return out, err
		}
		if p.ParentFamily != "" && !out.Recorded[p.ParentFamily] {
			out.Uncertain[p.ParentFamily] = true
		}
	}
	return out, ops.Err()
}

// YTArticleFamilies reads the tenant's YTArticleFamilyStates (read-only).
func (s *Store) YTArticleFamilies(ctx context.Context, tenant string) (YTArticleFamilyStates, error) {
	return ytArticleFamilyStatesTx(ctx, s.db(), tenant)
}

// YTArticleWaitingRoot reports whether a listed document is a long one that
// waits on its family's root: unbound on every tenant, in scope, its key in a
// family whose root is not recorded. The split waits on it (S13), so the
// document is shown with YTArticleWaitingFamilyRoot. It does not model a
// document whose policy amendment names its parent (a decision with amends:):
// those are rare and shown as their size class.
func YTArticleWaitingRoot(d YTArticleDoc, recorded map[string]bool) bool {
	family := YTArticleFamilyOf(d.Key)
	return d.BoundTenant == "" && family != "" && YTArticleSplit(d.BodyBytes) &&
		!recorded[family] && ytArticleInScope(d.Key, d.TaskID)
}

// PrepareYTArticleBackfill prepares document id's article op for the backfill
// (hk 5265) and returns its outcome: create_prepared, split_prepared,
// create_exists, not_updated (a long document with the split flag off),
// family_root_uncertain, or another outcome of prepareYTArticleTx. It runs
// the catch-up's own decision (prepareYTArticleTx) under the document's lane
// lock, with the backfill's two relaxations (ytDocsGate.backfill). A bound
// document is refused without an op: binding and updates are not the
// backfill's.
func (s *Store) PrepareYTArticleBackfill(ctx context.Context, id int64, now time.Time) (string, error) {
	var out ytArticleOutcome
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.prepareYTArticleBackfillOnce(ctx, now, id)
		return err
	})
	return string(out), err
}

// prepareYTArticleBackfillOnce is one attempt of PrepareYTArticleBackfill.
func (s *Store) prepareYTArticleBackfillOnce(ctx context.Context, now time.Time, id int64) (ytArticleOutcome, error) {
	g, on := s.ytDocsNow()
	if !on {
		return "", ErrYTArticleBackfillMode
	}
	g.backfill = true
	tx, release, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, ytwrite.ArticleEntity, strconv.FormatInt(id, 10)); err != nil {
		return "", err
	}
	r, err := readYTArticleRow(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if r != nil && r.bound {
		return ytArticleUnchanged, nil
	}
	if r != nil && !r.created && !r.splitOp {
		// A family whose root create is uncertain gets nothing new until the
		// create settles (hk 5265 item 3): its root is neither recorded nor
		// safe to create again.
		if family := YTArticleFamilyOf(r.doc.Key); family != "" && ytArticleAmends(r.doc.Key, r.body) == "" {
			states, err := ytArticleFamilyStatesTx(ctx, tx, g.tenant)
			if err != nil {
				return "", err
			}
			if states.Uncertain[family] && !states.Recorded[family] {
				return ytArticleFamilyUncertain, nil
			}
		}
	}
	outcome, err := prepareYTArticleTx(ctx, tx, now, g, id, r)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}

// ytArticlesWaitingRootTx lists, in tx, the long documents the split leaves
// waiting on a family root that is not recorded (YTArticleWaitingRoot), for
// the unwritten count and the authority refusal. It is called only while the
// split flag is on: with it off a long document is an exception, not a wait.
func ytArticlesWaitingRootTx(ctx context.Context, tx transaction, tenant string) ([]int64, error) {
	// LENGTH is MySQL's octet_length and `key` is quoted (hk 5077).
	query := `SELECT d.id, d.key, coalesce(d.task_id,0), octet_length(d.body) FROM documents d
		LEFT JOIN yt_article_bindings b ON b.document_id = d.id
		WHERE b.document_id IS NULL AND octet_length(d.body) > $1 AND d.created_at >= $2 ORDER BY d.id`
	args := []any{YTArticleSafePartBytes, YTImportEndedAt}
	if dialectFor(tx) == mysqlDialect {
		query = "SELECT d.id, d.`key`, coalesce(d.task_id,0), LENGTH(d.body) FROM documents d" + `
		LEFT JOIN yt_article_bindings b ON b.document_id = d.id
		WHERE b.document_id IS NULL AND LENGTH(d.body) > $1 AND d.created_at >= $2 ORDER BY d.id`
		query, args = ytMySQLArgs(query, args...)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var docs []YTArticleDoc
	for rows.Next() {
		var d YTArticleDoc
		if err := rows.Scan(&d.DocumentID, &d.Key, &d.TaskID, &d.BodyBytes); err != nil {
			rows.Close()
			return nil, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	states, err := ytArticleFamilyStatesTx(ctx, tx, tenant)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, d := range docs {
		if YTArticleWaitingRoot(d, states.Recorded) {
			ids = append(ids, d.DocumentID)
		}
	}
	return ids, nil
}

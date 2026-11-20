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
// its family has no recorded root in its target project: a split creates none
// (S13), so it waits until a single-article create of the family makes one.
const YTArticleWaitingFamilyRoot = "waiting_family_root"

// ytArticleFamilyUncertain is the outcome of a document whose family has a
// root create left uncertain (remote_unknown, no root recorded on its target
// project): nothing of that family there is enqueued until the create settles
// (hk 5265 item 3).
const ytArticleFamilyUncertain ytArticleOutcome = "family_root_uncertain"

// ytArticleCandidateChanged is the outcome of a backfill whose document no
// longer resolves to the bucket and YouTrack project it was verified in (its
// task moved or its mapping changed after classification): nothing is enqueued.
const ytArticleCandidateChanged ytArticleOutcome = "candidate_changed"

// ErrYTArticleBackfillMode is refused when the document mode is off: the gate
// admits no article op then, so the backfill prepares none.
var ErrYTArticleBackfillMode = errors.New("store: HANDOFFKEEP_YT_DOCS_MODE is off: the article backfill prepares no op")

// YTArticleEnqueueTarget is the candidate a backfill verified: the bucket the
// document was listed in and the YouTrack project id its article belongs in.
// PrepareYTArticleBackfill refuses it when the document resolves elsewhere now.
type YTArticleEnqueueTarget struct {
	Bucket    string
	ProjectID string
}

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

// ytFamilyKey is one family's root on one YouTrack project of a tenant. The
// root's identity is the tenant, project and family (yt_family_roots), so the
// state is keyed by project too: a root in one project says nothing about
// another. The separator is the unit separator, which no part can carry.
func ytFamilyKey(project, family string) string { return project + "\x1f" + family }

// YTArticleFamilyStates is what a tenant's family roots look like: whether the
// store recorded a root for a family on a project, and whether a root create
// on it was left uncertain (remote_unknown) with no root recorded since. The
// zero value knows no family.
type YTArticleFamilyStates struct {
	recorded  map[string]bool
	uncertain map[string]bool
}

// Recorded reports whether the store recorded a root for family on project.
func (f YTArticleFamilyStates) Recorded(project, family string) bool {
	return f.recorded[ytFamilyKey(project, family)]
}

// Uncertain reports whether a root create for family on project is
// remote_unknown and no root has been recorded for it.
func (f YTArticleFamilyStates) Uncertain(project, family string) bool {
	return f.uncertain[ytFamilyKey(project, family)]
}

// ytArticleFamilyStatesTx reads YTArticleFamilyStates for tenant on q (a
// read-only pool or a transaction).
func ytArticleFamilyStatesTx(ctx context.Context, q querier, tenant string) (YTArticleFamilyStates, error) {
	out := YTArticleFamilyStates{recorded: map[string]bool{}, uncertain: map[string]bool{}}
	rows, err := q.Query(ctx, sqlFor(q, `SELECT project_id, family FROM yt_family_roots WHERE tenant=$1`,
		`SELECT project_id, family FROM yt_family_roots WHERE tenant=?`), tenant)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var project, family string
		if err := rows.Scan(&project, &family); err != nil {
			rows.Close()
			return out, err
		}
		out.recorded[ytFamilyKey(project, family)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ops, err := q.Query(ctx, sqlFor(q, `SELECT yt_target_id, payload FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND tenant=$3 AND state='remote_unknown'`,
		`SELECT yt_target_id, payload FROM yt_write_ops WHERE principal=? AND kind=? AND tenant=? AND state='remote_unknown'`),
		YTDocsPrincipal, ytwrite.KindArticleCreate, tenant)
	if err != nil {
		return out, err
	}
	defer ops.Close()
	for ops.Next() {
		var project string
		var payload []byte
		if err := ops.Scan(&project, &payload); err != nil {
			return out, err
		}
		var p ytwrite.ArticleCreatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return out, err
		}
		if p.ParentFamily != "" && !out.recorded[ytFamilyKey(project, p.ParentFamily)] {
			out.uncertain[ytFamilyKey(project, p.ParentFamily)] = true
		}
	}
	return out, ops.Err()
}

// YTArticleFamilies reads the tenant's YTArticleFamilyStates (read-only).
func (s *Store) YTArticleFamilies(ctx context.Context, tenant string) (YTArticleFamilyStates, error) {
	return ytArticleFamilyStatesTx(ctx, s.db(), tenant)
}

// ytArticleRootWaits is whether prepareYTArticleTx would prepare a split for r
// that waits on its family's root on its target project: an unbound, in-scope
// long document with the split flag on, in a family, not legacy-mirrored (the
// legacy sync's article is never split), not amending a policy (its parent is
// that policy's article), and with no root recorded for the family on its
// project. The rule is date-free: the backfill admits long documents from
// before the import too, so the audit and the authority refusal count them.
func ytArticleRootWaits(g ytDocsGate, r *ytArticleRow, states YTArticleFamilyStates) bool {
	if r == nil || r.bound || !g.split || !YTArticleSplit(r.doc.BodyBytes) || !ytArticleInScope(r.doc.Key, r.doc.TaskID) {
		return false
	}
	if r.legacyArticle != "" || ytArticleAmends(r.doc.Key, r.body) != "" {
		return false
	}
	family := YTArticleFamilyOf(r.doc.Key)
	target, ok := ytArticleTarget(g, r.doc)
	if family == "" || !ok {
		return false
	}
	return !states.Recorded(target.ID, family)
}

// YTArticleRootWaits reports whether document id is a long document waiting on
// its family's root (ytArticleRootWaits) for tenant, with the buckets' projects
// as the gate would resolve them. Read-only.
func (s *Store) YTArticleRootWaits(ctx context.Context, tenant string, buckets map[string]YTArticleProject, id int64) (bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	r, err := readYTArticleRow(ctx, tx, id)
	if err != nil || r == nil {
		return false, err
	}
	states, err := ytArticleFamilyStatesTx(ctx, tx, tenant)
	if err != nil {
		return false, err
	}
	return ytArticleRootWaits(ytDocsGate{tenant: tenant, buckets: buckets, split: true}, r, states), nil
}

// YTArticleHasOp reports whether document id already has an article op
// (create or split) that holds its article: a backfill enqueue then adds
// nothing, which is what --limit counts. Read-only.
func (s *Store) YTArticleHasOp(ctx context.Context, id int64) (bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	r, err := readYTArticleRow(ctx, tx, id)
	if err != nil || r == nil {
		return false, err
	}
	return r.created || r.splitOp, nil
}

// PrepareYTArticleBackfill prepares document id's article op for the backfill
// (hk 5265) and returns its outcome: create_prepared, split_prepared,
// create_exists, not_updated (a long document with the split flag off),
// family_root_uncertain, candidate_changed, or another outcome of
// prepareYTArticleTx. want is the candidate the caller verified: when the
// document no longer lists in want.Bucket or resolves to another project,
// nothing is enqueued. The decision is prepareYTArticleTx's, under the
// document's lane lock, with the backfill's relaxations (ytDocsGate.backfill).
// A bound document is refused without an op: binding and updates are not the
// backfill's.
func (s *Store) PrepareYTArticleBackfill(ctx context.Context, id int64, now time.Time, want YTArticleEnqueueTarget) (string, error) {
	var out ytArticleOutcome
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.prepareYTArticleBackfillOnce(ctx, now, id, want)
		return err
	})
	return string(out), err
}

// prepareYTArticleBackfillOnce is one attempt of PrepareYTArticleBackfill.
func (s *Store) prepareYTArticleBackfillOnce(ctx context.Context, now time.Time, id int64, want YTArticleEnqueueTarget) (ytArticleOutcome, error) {
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
	if r != nil {
		if r.bound {
			return ytArticleUnchanged, nil
		}
		target, _ := ytArticleTarget(g, r.doc)
		if r.doc.Bucket != want.Bucket || target.ID != want.ProjectID {
			return ytArticleCandidateChanged, nil
		}
		// A family whose root create is uncertain gets nothing new on that
		// project until the create settles (hk 5265 item 3).
		if family := YTArticleFamilyOf(r.doc.Key); family != "" && !r.created && !r.splitOp && ytArticleAmends(r.doc.Key, r.body) == "" {
			states, err := ytArticleFamilyStatesTx(ctx, tx, g.tenant)
			if err != nil {
				return "", err
			}
			if states.Uncertain(target.ID, family) {
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

// ytArticlesWaitingRootTx lists, in tx, the long documents waiting on a family
// root (ytArticleRootWaits) for the unwritten count and the authority refusal.
// It is called only while the split flag is on: with it off a long document is
// an exception, not a wait. Every unbound long document is read, whatever its
// date, because the backfill splits the ones from before the import too.
func ytArticlesWaitingRootTx(ctx context.Context, tx transaction, g ytDocsGate) ([]int64, error) {
	// LENGTH is MySQL's octet_length (hk 5077).
	query := `SELECT d.id FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id = d.id
		WHERE b.document_id IS NULL AND octet_length(d.body) > $1 ORDER BY d.id`
	args := []any{YTArticleSafePartBytes}
	if dialectFor(tx) == mysqlDialect {
		query = `SELECT d.id FROM documents d LEFT JOIN yt_article_bindings b ON b.document_id = d.id
		WHERE b.document_id IS NULL AND LENGTH(d.body) > $1 ORDER BY d.id`
		query, args = ytMySQLArgs(query, args...)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	states, err := ytArticleFamilyStatesTx(ctx, tx, g.tenant)
	if err != nil {
		return nil, err
	}
	var waits []int64
	for _, id := range ids {
		r, err := readYTArticleRow(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if ytArticleRootWaits(g, r, states) {
			waits = append(waits, id)
		}
	}
	return waits, nil
}

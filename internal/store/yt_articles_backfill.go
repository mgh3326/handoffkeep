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

// ytArticleFamilyPending is the outcome of a short document whose family has
// an unresolved root-capable create on its project and no root recorded yet:
// the backfill enqueues one such create per family and project until the
// first one's root is recorded (or it ends rejected), so no second create can
// reach an uncertain root and make a second root.
const ytArticleFamilyPending ytArticleOutcome = "family_root_pending"

// ytArticleFamilyLock is the lockYTEntity kind of a family's root-capable
// creates on one project: a transaction-scoped lock keyed by tenant, project and
// family, taken before the family's pending state is read and the create
// inserted, so the check and the insert are atomic across documents (PR 528
// r4639407801, r4639418292). It is distinct from the worker's session family
// lane (ytwrite.Ledger.LockFamily), which covers the send.
const ytArticleFamilyLock = "yt_article_family"

// ytArticleCandidateChanged is the outcome of a backfill whose document no
// longer reads as the candidate it was verified as (its task moved, its
// mapping changed, or its body changed) once the locks are held: nothing is
// enqueued.
const ytArticleCandidateChanged ytArticleOutcome = "candidate_changed"

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

// ytFamilyKey is one family's root on one YouTrack project of a tenant. The
// root's identity is the tenant, project and family (yt_family_roots), so the
// state is keyed by project too: a root in one project says nothing about
// another. The separator is the unit separator, which no part can carry.
func ytFamilyKey(project, family string) string { return project + "\x1f" + family }

// YTArticleFamilyStates is what a tenant's family roots look like, per project
// and family: whether the store recorded a root; whether a root create is
// unresolved (prepared, dispatched or remote_unknown) and none is recorded
// yet (pending); and whether one is remote_unknown (uncertain). The zero value
// knows no family.
type YTArticleFamilyStates struct {
	recorded  map[string]bool
	pending   map[string]bool
	uncertain map[string]bool
}

// Recorded reports whether the store recorded a root for family on project.
func (f YTArticleFamilyStates) Recorded(project, family string) bool {
	return f.recorded[ytFamilyKey(project, family)]
}

// Pending reports whether an unresolved root-capable create for family on
// project exists (it may still record the root).
func (f YTArticleFamilyStates) Pending(project, family string) bool {
	return f.pending[ytFamilyKey(project, family)]
}

// Uncertain reports whether a root create for family on project is
// remote_unknown and no root has been recorded for it.
func (f YTArticleFamilyStates) Uncertain(project, family string) bool {
	return f.uncertain[ytFamilyKey(project, family)]
}

// ytArticleFamilyStatesTx reads YTArticleFamilyStates for tenant on q (a
// read-only pool or a transaction).
func ytArticleFamilyStatesTx(ctx context.Context, q querier, tenant string) (YTArticleFamilyStates, error) {
	out := YTArticleFamilyStates{recorded: map[string]bool{}, pending: map[string]bool{}, uncertain: map[string]bool{}}
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
	ops, err := q.Query(ctx, sqlFor(q, `SELECT yt_target_id, payload, state FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND tenant=$3 AND state IN ('prepared','dispatched','remote_unknown')`,
		`SELECT yt_target_id, payload, state FROM yt_write_ops WHERE principal=? AND kind=? AND tenant=? AND state IN ('prepared','dispatched','remote_unknown')`),
		YTDocsPrincipal, ytwrite.KindArticleCreate, tenant)
	if err != nil {
		return out, err
	}
	defer ops.Close()
	for ops.Next() {
		var project, state string
		var payload []byte
		if err := ops.Scan(&project, &payload, &state); err != nil {
			return out, err
		}
		var p ytwrite.ArticleCreatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return out, err
		}
		if p.ParentFamily == "" {
			continue
		}
		key := ytFamilyKey(project, p.ParentFamily)
		out.pending[key] = true
		if state == "remote_unknown" {
			out.uncertain[key] = true
		}
	}
	if err := ops.Err(); err != nil {
		return out, err
	}
	// A recorded root settles its family: pending and uncertain are about a
	// root not yet recorded.
	for key := range out.recorded {
		delete(out.pending, key)
		delete(out.uncertain, key)
	}
	return out, nil
}

// YTArticleFamilies reads the tenant's YTArticleFamilyStates (read-only).
func (s *Store) YTArticleFamilies(ctx context.Context, tenant string) (YTArticleFamilyStates, error) {
	var out YTArticleFamilyStates
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = ytArticleFamilyStatesTx(ctx, s.db(), tenant)
		return err
	})
	return out, err
}

// ytArticleSplitEligible is whether prepareYTArticleTx would prepare a split
// for r: an unbound, in-scope long document with the split flag on, not
// legacy-mirrored (the legacy sync's article is never split). A document that
// amends a policy is eligible too: its split goes under the policy's article.
// The rule is date-free: the backfill admits long documents from before the
// import too.
func ytArticleSplitEligible(g ytDocsGate, r *ytArticleRow) bool {
	return r != nil && !r.bound && g.split && YTArticleSplit(r.doc.BodyBytes) && ytArticleInScope(r.doc.Key, r.doc.TaskID) &&
		r.legacyArticle == ""
}

// ytArticleRootWaits is whether an eligible long document waits on its
// family's root on its target project: the split creates no root (S13), so it
// waits while none is recorded for its family there. A document that amends a
// policy waits on nothing family-wide: its parent is the policy's article.
func ytArticleRootWaits(g ytDocsGate, r *ytArticleRow, states YTArticleFamilyStates) bool {
	if !ytArticleSplitEligible(g, r) || ytArticleAmends(r.doc.Key, r.body) != "" {
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
	var waits bool
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		r, err := readYTArticleRow(ctx, tx, id)
		if err != nil || r == nil {
			return err
		}
		states, err := ytArticleFamilyStatesTx(ctx, tx, tenant)
		if err != nil {
			return err
		}
		waits = ytArticleRootWaits(ytDocsGate{tenant: tenant, buckets: buckets, split: true}, r, states)
		return nil
	})
	return waits, err
}

// YTArticleHasOp reports whether document id already has an article op
// (create or split) that holds its article: a backfill enqueue then adds
// nothing, which is what --limit counts. Read-only.
func (s *Store) YTArticleHasOp(ctx context.Context, id int64) (bool, error) {
	var has bool
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		r, err := readYTArticleRow(ctx, tx, id)
		if err != nil || r == nil {
			return err
		}
		has = r.created || r.splitOp
		return nil
	})
	return has, err
}

// PrepareYTArticleBackfill prepares document id's article op for the backfill
// (hk 5265) and returns its outcome: create_prepared, split_prepared,
// create_exists, not_updated (a long document with the split flag off),
// family_root_uncertain, family_root_pending, candidate_changed, or another
// outcome of prepareYTArticleTx. verified is the candidate the caller listed
// and classified: once the locks are held, a document that no longer reads as
// it is enqueues nothing.
//
// The locks are AddYTArticleBinding's, in its order (lockorder.go): the
// article entity lock, the documents authority share, then FOR SHARE on the
// document, its task and its project mapping. A task or mapping move committed
// before those locks is seen by the read below (candidate_changed); one that
// tries after them waits for this transaction, so the op lands on the verified
// target (PR 528 r4639004015). The re-check and the op insert are one
// transaction, on PostgreSQL and MySQL.
//
// A bound document is refused without an op: binding and updates are not the
// backfill's.
func (s *Store) PrepareYTArticleBackfill(ctx context.Context, id int64, now time.Time, verified YTArticleCandidate) (string, error) {
	var out ytArticleOutcome
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.prepareYTArticleBackfillOnce(ctx, now, id, verified)
		return err
	})
	return string(out), err
}

// prepareYTArticleBackfillOnce is one attempt of PrepareYTArticleBackfill.
func (s *Store) prepareYTArticleBackfillOnce(ctx context.Context, now time.Time, id int64, verified YTArticleCandidate) (ytArticleOutcome, error) {
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
	// The same share AddYTArticleBinding takes: a documents authority switch
	// (FOR UPDATE) either commits first or waits for this op.
	if _, err := ytAuthorityShareTx(ctx, tx, YTAuthorityDocuments); err != nil {
		return "", err
	}
	if err := lockYTArticleSourcesTx(ctx, tx, id); err != nil {
		if errors.Is(err, ErrYTArticleBindingConflict) {
			return ytArticleCandidateChanged, nil
		}
		return "", err
	}
	r, err := readYTArticleRow(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if r == nil || r.doc != verified {
		return ytArticleCandidateChanged, nil
	}
	if r.bound {
		return ytArticleUnchanged, nil
	}
	if family := YTArticleFamilyOf(r.doc.Key); family != "" && ytArticleAmends(r.doc.Key, r.body) == "" && !r.created && !r.splitOp {
		target, _ := ytArticleTarget(g, r.doc)
		if !YTArticleSplit(r.doc.BodyBytes) {
			// The short document's root-capable create: serialize its family on
			// this project before reading the pending state.
			if err := lockYTEntity(ctx, tx, ytArticleFamilyLock, g.tenant+"\x1f"+target.ID+"\x1f"+family); err != nil {
				return "", err
			}
		}
		states, err := ytArticleFamilyStatesTx(ctx, tx, g.tenant)
		if err != nil {
			return "", err
		}
		if states.Uncertain(target.ID, family) {
			return ytArticleFamilyUncertain, nil
		}
		// One root-capable create per family and project until its root is
		// recorded: a split creates no root, so only a short document waits here.
		if !YTArticleSplit(r.doc.BodyBytes) && states.Pending(target.ID, family) && !states.Recorded(target.ID, family) {
			return ytArticleFamilyPending, nil
		}
	}
	outcome, err := prepareYTArticleTx(ctx, tx, now, g, id, r)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}

// ytDocumentOpActiveTx reports whether document id has an unresolved hk
// documents op (prepared, dispatched or remote_unknown) in tx: the write the
// authority switch must wait for.
func ytDocumentOpActiveTx(ctx context.Context, tx transaction, id int64) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, sqlFor(tx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$1 AND entity_kind='document' AND entity_id=$2 AND state IN ('prepared','dispatched','remote_unknown'))`,
		`SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=? AND entity_kind='document' AND entity_id=? AND state IN ('prepared','dispatched','remote_unknown'))`),
		YTDocsPrincipal, strconv.FormatInt(id, 10)).Scan(&active)
	return active, err
}

// ytArticlesLongUnwrittenTx lists, in tx, the long documents the split must
// write but does not, with the split flag on: every eligible one
// (ytArticleSplitEligible) that waits on its family's root (any op state: its
// split cannot send until the root exists), and every eligible one with its
// root in place and no unresolved op. waiting counts the first kind. With the
// flag off a long document is an exception, not a write, so nothing is listed.
func ytArticlesLongUnwrittenTx(ctx context.Context, tx transaction, g ytDocsGate) (ids []int64, waiting int, err error) {
	if !g.split {
		return nil, 0, nil
	}
	// LENGTH is MySQL's octet_length (hk 5077). Every unbound long document is
	// read, whatever its date: the backfill splits the ones from before the import too.
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
		return nil, 0, err
	}
	var long []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		long = append(long, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(long) == 0 {
		return nil, 0, nil
	}
	states, err := ytArticleFamilyStatesTx(ctx, tx, g.tenant)
	if err != nil {
		return nil, 0, err
	}
	for _, id := range long {
		r, err := readYTArticleRow(ctx, tx, id)
		if err != nil {
			return nil, 0, err
		}
		if !ytArticleSplitEligible(g, r) {
			continue
		}
		if ytArticleRootWaits(g, r, states) {
			ids = append(ids, id)
			waiting++
			continue
		}
		active, err := ytDocumentOpActiveTx(ctx, tx, id)
		if err != nil {
			return nil, 0, err
		}
		if !active {
			ids = append(ids, id)
		}
	}
	return ids, waiting, nil
}

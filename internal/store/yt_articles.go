package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Document articles (hk 4206, docs/youtrack-write-protocol.md "Kinds
// article_create and article_update"): after the import ended, documents are
// canonical in YouTrack and reach their articles only through this path. The
// change-feed consumer document_articles reads every document change and, in
// mode on, for each document in scope (ytArticleInScope):
//
//   - bound (yt_article_bindings on the gate's tenant) and its body is not
//     the one the article holds (content_sha differs, or needs_update): one
//     article_update op per changed snapshot (prepareYTSnapshotTx);
//   - unbound and created after the import ended (YTImportEndedAt): one
//     article_create op, whose apply publishes the binding (origin
//     op_applied);
//   - unbound and created before: exception_unbound_imported, no op (the
//     import made, or refused to bind, its article: a summary_cut document
//     lands here);
//   - over YTArticleSafePartBytes (the import split it), or bound to a split
//     document's index: exception_not_updated, no op (D4, no Y5c now).
//
// The ops ride the document's lane (document:<id>) under principal
// hk:documents. No parent is set (hk 3995 has no settled rule).

// YTDocsMode gates the document articles (HANDOFFKEEP_YT_DOCS_MODE).
type YTDocsMode string

const (
	// YTDocsOff prepares no document op and admits none (the default). The
	// consumer's cursor still advances: a document changed while off
	// reaches YouTrack with its next change.
	YTDocsOff YTDocsMode = "off"
	// YTDocsOn prepares document ops from the feed and admits them.
	YTDocsOn YTDocsMode = "on"
)

// YTDocsPrincipal owns (and is the actor of) the document ops.
const YTDocsPrincipal = "hk:documents"

// ytDocsConsumer is the change-feed consumer's name (its cursor's key).
const ytDocsConsumer = "document_articles"

// YTImportEndedAt is when the import ended (operator, hk 4206): a document
// created before it had its article made by the import, and one created
// after has none until article_create makes it.
var YTImportEndedAt = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// The exception classes of an in-scope document: written by no op, counted
// by the feed's log and by `youtrack audit`.
const (
	// YTArticleExceptionNotUpdated: the body is over YTArticleSafePartBytes,
	// or the binding is a split document's index (D4).
	YTArticleExceptionNotUpdated = "exception_not_updated"
	// YTArticleExceptionUnboundImported: no binding on the tenant, and the
	// document was created before the import ended.
	YTArticleExceptionUnboundImported = "exception_unbound_imported"
)

// YTArticleProject is a YouTrack project an article is created in.
type YTArticleProject struct {
	ID        string
	ShortName string
}

type ytDocsGate struct {
	mode   YTDocsMode
	tenant string
	// buckets are the YouTrack projects of /shared and /unassigned.
	buckets map[string]YTArticleProject
}

// SetYTDocsMode sets the gate for this process: the tenant, and the YouTrack
// projects of the two synthetic buckets (YTArticleBucketShared,
// YTArticleBucketUnassigned) a new document's article is created in when it
// has no task or its task no project. serve calls it once at start. On with
// an empty tenant or a bucket missing is off.
func (s *Store) SetYTDocsMode(m YTDocsMode, tenant string, buckets map[string]YTArticleProject) {
	g := &ytDocsGate{mode: m, tenant: tenant, buckets: map[string]YTArticleProject{}}
	for k, v := range buckets {
		g.buckets[k] = v
	}
	s.ytDocs.Store(g)
}

// ytDocsNow is the gate: on with its tenant and both buckets, or off.
func (s *Store) ytDocsNow() (ytDocsGate, bool) {
	g := s.ytDocs.Load()
	if g == nil || g.mode != YTDocsOn || g.tenant == "" {
		return ytDocsGate{mode: YTDocsOff}, false
	}
	for _, b := range []string{YTArticleBucketShared, YTArticleBucketUnassigned} {
		if p := g.buckets[b]; p.ID == "" || p.ShortName == "" {
			return ytDocsGate{mode: YTDocsOff}, false
		}
	}
	return *g, true
}

func init() {
	registerYTKindGate(ytwrite.KindArticleCreate, ytArticleCreateGateSQL)
	registerYTKindGate(ytwrite.KindArticleUpdate, ytArticleUpdateGateSQL)
	registerYTApplyHook(ytwrite.KindArticleCreate, ytApplyHook{conflict: ytArticleConflictTx, bind: bindYTArticleApplyTx})
	registerYTApplyHook(ytwrite.KindArticleUpdate, ytApplyHook{conflict: noYTConflict, bind: bindYTArticleUpdateApplyTx})
	registerYTFeedStoreConsumer(ytDocsConsumer, []string{"document"}, ytDocsHandler)
}

// ytArticleCreateGateSQL is the article_create gate (ytAdmitSQL): mode on,
// and o's document has no binding or one this op published. A backfill
// binding holds the op: the document already has its article.
func ytArticleCreateGateSQL(s *Store) string {
	if _, on := s.ytDocsNow(); !on {
		return `FALSE`
	}
	return `o.entity_kind = 'document' AND NOT EXISTS (SELECT 1 FROM yt_article_bindings b
		WHERE b.document_id::text = o.entity_id AND b.op_id IS DISTINCT FROM o.id)`
}

// ytArticleUpdateGateSQL is the article_update gate: mode on, and o's
// target is the article o's document is bound to on o's tenant now.
func ytArticleUpdateGateSQL(s *Store) string {
	if _, on := s.ytDocsNow(); !on {
		return `FALSE`
	}
	return `o.entity_kind = 'document' AND EXISTS (SELECT 1 FROM yt_article_bindings b
		WHERE b.document_id::text = o.entity_id AND b.article_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// ytArticleInScope reports whether the document kinds write the document:
// every document the import made an article for (YTArticleExcluded false).
// That leaves out screens/*, review/mwr-* and a task's reports, which Y5d
// posts as comments; a task's brief gets its article here and Y5d's pointer
// comment names it (handoffkeep-design, hk 4206). taskID 0 is no task.
func ytArticleInScope(key string, taskID int64) bool { return !YTArticleExcluded(key, taskID) }

// YTArticleInScope is ytArticleInScope for the audit.
func YTArticleInScope(key string, taskID int64) bool { return ytArticleInScope(key, taskID) }

// ytArticleException is the exception class of an in-scope document ("" for
// none): bodyBytes is its body's length, bound whether it has a binding on
// the tenant, splitIndex whether that binding is a split document's index
// (no content_sha), createdAt when the document was created.
func ytArticleException(bodyBytes int, bound, splitIndex bool, createdAt time.Time) string {
	switch {
	case YTArticleSplit(bodyBytes), bound && splitIndex:
		return YTArticleExceptionNotUpdated
	case !bound && createdAt.Before(YTImportEndedAt):
		return YTArticleExceptionUnboundImported
	}
	return ""
}

// YTArticleExceptionOf is the exception class of a listed document on
// tenant ("" for none, or for a document out of scope).
func YTArticleExceptionOf(d YTArticleDoc, tenant string) string {
	if !ytArticleInScope(d.Key, d.TaskID) {
		return ""
	}
	bound := d.BoundTenant != "" && d.BoundTenant == tenant
	return ytArticleException(d.BodyBytes, bound, bound && d.BoundSplitIndex, d.CreatedAt)
}

// ytArticleRow is one document as the document kinds read it: the row, its
// article project, its binding and whether its article_create op exists.
type ytArticleRow struct {
	doc       YTArticleCandidate
	body      string
	createdAt time.Time
	binding   YTArticleBinding
	bound     bool
	created   bool
}

// readYTArticleRow reads document id; nil when it is gone.
func readYTArticleRow(ctx context.Context, tx pgx.Tx, id int64) (*ytArticleRow, error) {
	r := &ytArticleRow{}
	err := scanYTArticleCandidate(tx.QueryRow(ctx, ytArticleCandidateSQL+`, d.body, d.created_at,
			EXISTS(SELECT 1 FROM yt_write_ops o WHERE o.principal = $2 AND o.kind = $3 AND o.idem_key = $4)`+ytArticleCandidateFrom+` WHERE d.id = $1`,
		id, YTDocsPrincipal, ytwrite.KindArticleCreate, ytwrite.ArticleKey(id)), &r.doc, &r.body, &r.createdAt, &r.created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.createdAt = r.createdAt.UTC()
	r.binding, r.bound, err = readYTArticleBinding(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ytArticleOutcome is what prepareYTArticleTx did with one document.
type ytArticleOutcome string

const (
	ytArticleDeleted         ytArticleOutcome = "deleted"
	ytArticleOutOfScope      ytArticleOutcome = "out_of_scope"
	ytArticleNotUpdated      ytArticleOutcome = YTArticleExceptionNotUpdated
	ytArticleUnboundImported ytArticleOutcome = YTArticleExceptionUnboundImported
	ytArticleOtherTenant     ytArticleOutcome = "other_tenant"
	ytArticleUnchanged       ytArticleOutcome = "unchanged"
	ytArticleUpdatePrepared  ytArticleOutcome = "update_prepared"
	ytArticleCreatePrepared  ytArticleOutcome = "create_prepared"
	ytArticleCreateRejected  ytArticleOutcome = "create_rejected"
	ytArticleCreatePending   ytArticleOutcome = "create_exists"
	ytArticleNoProject       ytArticleOutcome = "no_project"
	ytArticleInvalid         ytArticleOutcome = "invalid"
)

// ytArticleTarget is the YouTrack project a new document's article is
// created in: a synthetic bucket's from the gate, else its hk project's
// youtrack_projects row; ok is false when there is none.
func ytArticleTarget(g ytDocsGate, c YTArticleCandidate) (YTArticleProject, bool) {
	if p, ok := g.buckets[c.Bucket]; ok {
		return p, true
	}
	if c.YTProjectID == "" || c.ShortName == "" {
		return YTArticleProject{}, false
	}
	return YTArticleProject{ID: c.YTProjectID, ShortName: c.ShortName}, true
}

// prepareYTArticleTx decides one document in tx at now for gate g, in this
// order: deleted, out of scope, an exception (ytArticleException), a binding
// on another tenant (skipped), then a bound document's update, prepared
// through prepareYTSnapshotTx unless its body is the one the article holds;
// or an unbound document's create, unless it already has one (in any state:
// a rejected create is the operator's). A document whose snapshot the
// protocol cannot store is skipped and logged.
func prepareYTArticleTx(ctx context.Context, tx pgx.Tx, now time.Time, g ytDocsGate, id int64, r *ytArticleRow) (ytArticleOutcome, error) {
	if r == nil {
		return ytArticleDeleted, nil
	}
	if !ytArticleInScope(r.doc.Key, r.doc.TaskID) {
		return ytArticleOutOfScope, nil
	}
	if r.bound && r.binding.Tenant != g.tenant {
		return ytArticleOtherTenant, nil
	}
	if ex := ytArticleException(r.doc.BodyBytes, r.bound, r.binding.ContentSHA == "", r.createdAt); ex != "" {
		return ytArticleOutcome(ex), nil
	}
	summary := YTArticleSummary(r.doc.Key, r.doc.Kind)
	if r.bound {
		if !r.binding.NeedsUpdate && r.binding.ContentSHA == r.doc.SHA256 {
			return ytArticleUnchanged, nil
		}
		n, err := ytwrite.ArticleUpdateOp(ytwrite.ArticleUpdateRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal,
			Tenant: r.binding.Tenant, ArticleID: r.binding.ArticleID, DocumentID: id, Summary: summary, Content: r.body})
		if errors.Is(err, ytwrite.ErrInvalid) {
			// Bad data must not stop the feed: the cursor would never pass
			// this row.
			log.Printf("ytwrite.article_update_skipped document_id=%d reason=invalid err=%q", id, err.Error())
			return ytArticleInvalid, nil
		}
		if err != nil {
			return "", err
		}
		op, made, err := prepareYTSnapshotTx(ctx, tx, now, n, n.PayloadSHA)
		if err != nil {
			return "", err
		}
		if !made {
			return ytArticleUnchanged, nil
		}
		log.Printf("ytwrite.article_update_enqueued op_id=%d document_id=%d article_id=%q", op.ID, id, r.binding.ArticleID)
		return ytArticleUpdatePrepared, nil
	}
	if r.created {
		return ytArticleCreatePending, nil
	}
	target, ok := ytArticleTarget(g, r.doc)
	if !ok {
		log.Printf("ytwrite.article_create_skipped document_id=%d reason=no_project project=%q", id, r.doc.Bucket)
		return ytArticleNoProject, nil
	}
	req := ytwrite.ArticleRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal, YTProjectID: target.ID, YTProjectShort: target.ShortName,
		Tenant: g.tenant, DocumentID: id, Summary: summary, Body: r.body}
	n, err := ytwrite.ArticleOp(req)
	if err != nil {
		return "", err
	}
	op, created, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil {
		return "", err
	}
	if !created {
		return ytArticleCreatePending, nil
	}
	if cerr := ytwrite.CheckArticle(req); cerr != nil {
		if _, err := rejectYTOpTx(ctx, tx, now, op.ID, ytwrite.ReasonInvalidIssue, cerr.Error()); err != nil {
			return "", err
		}
		log.Printf("ytwrite.article_create_rejected op_id=%d document_id=%d err=%q", op.ID, id, cerr.Error())
		return ytArticleCreateRejected, nil
	}
	log.Printf("ytwrite.article_create_enqueued op_id=%d document_id=%d project_id=%q", op.ID, id, target.ID)
	return ytArticleCreatePrepared, nil
}

// articleFeedIDs are the document ids of the feed rows, in order.
func articleFeedIDs(rows []FeedRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.Model != "document" {
			continue
		}
		if id, err := strconv.ParseInt(r.ModelID, 10, 64); err == nil && id > 0 && strconv.FormatInt(id, 10) == r.ModelID {
			ids = append(ids, id)
		}
	}
	return ids
}

// ytDocsHandler is the document_articles consumer's handler for one pass at
// now. Mode off does nothing (the cursor advances). Otherwise each changed
// document, in sync_id order, is read under its lane lock and goes through
// prepareYTArticleTx.
func ytDocsHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		g, on := s.ytDocsNow()
		if !on {
			return nil
		}
		ids := articleFeedIDs(rows)
		if len(ids) == 0 {
			return nil
		}
		counts := map[ytArticleOutcome]int{}
		for _, id := range ids {
			// Read under the lane lock a create's Apply publishes the
			// binding under: a binding committed before this read is seen,
			// and one after it waits for this pass.
			if err := lockYTEntity(ctx, tx, ytwrite.ArticleEntity, strconv.FormatInt(id, 10)); err != nil {
				return err
			}
			r, err := readYTArticleRow(ctx, tx, id)
			if err != nil {
				return err
			}
			outcome, err := prepareYTArticleTx(ctx, tx, now, g, id, r)
			if err != nil {
				return err
			}
			counts[outcome]++
		}
		log.Printf("ytwrite.articles_feed documents=%d create_prepared=%d create_rejected=%d create_exists=%d update_prepared=%d unchanged=%d exception_not_updated=%d exception_unbound_imported=%d out_of_scope=%d deleted=%d other_tenant=%d no_project=%d invalid=%d",
			len(ids), counts[ytArticleCreatePrepared], counts[ytArticleCreateRejected], counts[ytArticleCreatePending], counts[ytArticleUpdatePrepared],
			counts[ytArticleUnchanged], counts[ytArticleNotUpdated], counts[ytArticleUnboundImported], counts[ytArticleOutOfScope],
			counts[ytArticleDeleted], counts[ytArticleOtherTenant], counts[ytArticleNoProject], counts[ytArticleInvalid])
		return nil
	}
}

// ytArticleConflictTx reads, in Apply's transaction (which holds the
// document's lane lock), whether publishing article ytID for op's document
// would contradict a mapping hk already keeps: the document bound or mapped
// to another article, or the article bound or mapped to another document.
// It returns the conflict ("" when none) and logs it as
// ytwrite.binding_conflict.
func ytArticleConflictTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string) (string, error) {
	id, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || id < 1 || op.EntityKind != ytwrite.ArticleEntity {
		return "", fmt.Errorf("article_create op %d is not on a document lane (%s:%s)", op.ID, op.EntityKind, op.EntityID)
	}
	var detail string
	err = tx.QueryRow(ctx, `SELECT coalesce(
		(SELECT 'document is bound to article '||article_id||' on '||tenant||' ('||origin||')' FROM yt_article_bindings
			WHERE document_id=$1 AND (tenant<>$2 OR article_id<>$3)),
		(SELECT 'article is bound to document '||document_id FROM yt_article_bindings
			WHERE tenant=$2 AND article_id=$3 AND document_id<>$1 LIMIT 1),
		(SELECT 'document is mapped to article '||yt_id FROM yt_entity_map
			WHERE entity_kind='document' AND hk_key=$4 AND yt_id<>$3),
		(SELECT 'article is mapped to document '||hk_key FROM yt_entity_map
			WHERE entity_kind='document' AND yt_id=$3 AND hk_key<>$4),
		'')`, id, op.Tenant, ytID, op.EntityID).Scan(&detail)
	if err != nil {
		return "", err
	}
	if detail != "" {
		log.Printf("ytwrite.binding_conflict document_id=%d op_id=%d article_id=%q detail=%q", id, op.ID, ytID, detail)
	}
	return detail, nil
}

// bindYTArticleApplyTx is the article_create apply hook's bind: the
// document's binding (origin op_applied, this op, part 0) with content_sha
// the sha256 of the body the op wrote, then, as the feed would, the
// document's update when its body changed since the op was prepared (or the
// article was adopted, whose content the op did not write: needs_update). A
// binding of the document to this same article is kept as it is.
func bindYTArticleApplyTx(s *Store, ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, adopted bool) error {
	id, _ := strconv.ParseInt(op.EntityID, 10, 64)
	var p ytwrite.ArticleCreatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return fmt.Errorf("payload of op %d: %w", op.ID, err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO yt_article_bindings(document_id, tenant, article_id, article_readable, project_id, part, generation, content_sha, origin, op_id, needs_update, bound_by, bound_at)
		VALUES ($1,$2,$3,'',$4,0,0,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		id, op.Tenant, ytID, op.YTTargetID, ytwrite.ContentSHA(p.Body), YTBindingOpApplied, op.ID, adopted, op.Principal, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		have, found, err := readYTArticleBinding(ctx, tx, id)
		if err != nil {
			return err
		}
		if !found || have.Tenant != op.Tenant || have.ArticleID != ytID {
			// Another document's binding took the article after the
			// conflict check: roll the apply back; the next check records
			// it.
			return fmt.Errorf("%w: article %s on %s was bound to another document during the apply of op %d", ytwrite.ErrConflictingResult, ytID, op.Tenant, op.ID)
		}
	}
	outcome, err := s.ytArticleSnapshotTx(ctx, tx, id, now)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.article_on_bind document_id=%d outcome=%q", id, outcome)
	return nil
}

// ytArticleSnapshotTx prepares document id's update in tx when the mode is
// on and it is bound on the gate's tenant (no op for an unchanged body). The
// caller holds the document's lane lock.
func (s *Store) ytArticleSnapshotTx(ctx context.Context, tx pgx.Tx, id int64, now time.Time) (ytArticleOutcome, error) {
	g, on := s.ytDocsNow()
	if !on {
		return "", nil
	}
	r, err := readYTArticleRow(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if r != nil && !r.bound {
		return "", nil
	}
	return prepareYTArticleTx(ctx, tx, now, g, id, r)
}

// noYTConflict is the conflict check of an apply hook that publishes no new
// mapping.
func noYTConflict(context.Context, pgx.Tx, ytwrite.Op, string) (string, error) { return "", nil }

// bindYTArticleUpdateApplyTx records, with an applied article_update, that
// the bound article holds the op's content: content_sha becomes its sha256
// and needs_update false, so the feed prepares no update until the body
// changes again. A binding that no longer names the op's article (none is
// ever overwritten, so only a binding removed by hand) is left alone.
func bindYTArticleUpdateApplyTx(_ *Store, ctx context.Context, tx pgx.Tx, op ytwrite.Op, _ string, _ time.Time, _ bool) error {
	var p ytwrite.ArticleUpdatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return fmt.Errorf("payload of op %d: %w", op.ID, err)
	}
	_, err := tx.Exec(ctx, `UPDATE yt_article_bindings SET content_sha=$4, needs_update=false
		WHERE document_id::text=$1 AND tenant=$2 AND article_id=$3`, op.EntityID, op.Tenant, op.YTTargetID, ytwrite.ContentSHA(p.Content))
	return err
}

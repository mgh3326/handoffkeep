package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Decision mode (hk 4054, docs/youtrack-write-protocol.md "Kinds
// decision_create and decision_fields"): a decision request's DEC issue
// carries its whole text. The change-feed consumer decision_issues reads every
// decision_requests write (the "record:<id>" decision rows,
// migration_decision_record_feed.go) and, in mode on, prepares the decision's
// decision_create op while it has no binding, or its decision_fields snapshot
// once it has one (yt_decision_bindings). Applying a decision_create op
// publishes the binding (origin op_applied) and prepares the first snapshot
// in the same transaction; `youtrack decisions-backfill` binds the imported
// DEC-<id> issues (origin import_verified) after verifying them. The ops ride
// the decision's lane (decision:<id>). Arbitration, receipts, option ids and
// question hashes stay in PG.

// YTDecisionsMode gates the decision projection (HANDOFFKEEP_YT_DECISIONS_MODE).
type YTDecisionsMode string

const (
	// YTDecisionsOff prepares no decision op and admits none (the default).
	// The consumer's cursor still advances: a change made while off reaches
	// YouTrack with the decision's next change.
	YTDecisionsOff YTDecisionsMode = "off"
	// YTDecisionsOn prepares decision ops from the feed and admits them.
	YTDecisionsOn YTDecisionsMode = "on"
)

// YTDecisionsPrincipal owns (and is the actor of) the decision ops: the
// create's key is ytwrite.DecisionKey(id), a snapshot's SnapshotKey.
const YTDecisionsPrincipal = "hk:decisions"

// ytDecisionsConsumer is the change-feed consumer's name (its cursor's key).
const ytDecisionsConsumer = "decision_issues"

type ytDecisionsGate struct {
	mode                         YTDecisionsMode
	tenant, projectID, shortName string
}

// SetYTDecisionsMode sets the gate for this process: the YouTrack tenant and
// the DEC project (its id and short name) a new decision's issue is created
// in. serve calls it once at start. On with an empty tenant or project is
// off.
func (s *Store) SetYTDecisionsMode(m YTDecisionsMode, tenant, projectID, shortName string) {
	s.ytDecisions.Store(&ytDecisionsGate{mode: m, tenant: tenant, projectID: projectID, shortName: shortName})
}

// ytDecisionsNow is the gate: on with its tenant and project, or off.
func (s *Store) ytDecisionsNow() (ytDecisionsGate, bool) {
	g := s.ytDecisions.Load()
	if g == nil || g.mode != YTDecisionsOn || g.tenant == "" || g.projectID == "" || g.shortName == "" {
		return ytDecisionsGate{mode: YTDecisionsOff}, false
	}
	return *g, true
}

func init() {
	registerYTKindGate(ytwrite.KindDecisionCreate, ytDecisionCreateGateSQL)
	registerYTKindGate(ytwrite.KindDecisionFields, ytDecisionFieldsGateSQL)
	registerYTApplyHook(ytwrite.KindDecisionCreate, ytApplyHook{conflict: ytDecisionConflictTx, bind: bindYTDecisionApplyTx})
	registerYTFeedStoreConsumer(ytDecisionsConsumer, []string{"decision"}, ytDecisionsHandler)
}

// ytDecisionCreateGateSQL is the decision_create gate (ytAdmitSQL): mode on,
// and o's decision has no binding or one this op published. A binding from
// the backfill holds the op: the decision already has its issue.
func ytDecisionCreateGateSQL(s *Store) string {
	if _, on := s.ytDecisionsNow(); !on {
		return `FALSE`
	}
	return `o.entity_kind = 'decision' AND NOT EXISTS (SELECT 1 FROM yt_decision_bindings b
		WHERE b.decision_id = o.entity_id AND b.op_id IS DISTINCT FROM o.id)`
}

// ytDecisionFieldsGateSQL is the decision_fields gate: mode on, and o's
// target is the issue o's decision is bound to on o's tenant now.
func ytDecisionFieldsGateSQL(s *Store) string {
	if _, on := s.ytDecisionsNow(); !on {
		return `FALSE`
	}
	return `o.entity_kind = 'decision' AND EXISTS (SELECT 1 FROM yt_decision_bindings b
		WHERE b.decision_id = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// YTDecisionBinding is one decision's DEC issue.
type YTDecisionBinding struct {
	DecisionID    int64     `json:"decision_id"`
	Tenant        string    `json:"tenant"`
	IssueID       string    `json:"issue_id"`
	IssueReadable string    `json:"issue_readable,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	Origin        string    `json:"origin"`
	OpID          int64     `json:"op_id,omitempty"`
	BoundBy       string    `json:"bound_by"`
	BoundAt       time.Time `json:"bound_at"`
}

const ytDecisionBindingColumns = `decision_id::bigint, tenant, issue_id, issue_readable, project_id, origin, coalesce(op_id,0), bound_by, bound_at`

func scanYTDecisionBinding(row pgx.Row, b *YTDecisionBinding) error {
	if err := row.Scan(&b.DecisionID, &b.Tenant, &b.IssueID, &b.IssueReadable, &b.ProjectID, &b.Origin, &b.OpID, &b.BoundBy, &b.BoundAt); err != nil {
		return err
	}
	b.BoundAt = b.BoundAt.UTC()
	return nil
}

// YTDecisionBinding reads a decision's binding; found is false when it has
// none.
func (s *Store) YTDecisionBinding(ctx context.Context, decisionID int64) (YTDecisionBinding, bool, error) {
	return readYTDecisionBinding(ctx, s.pool, decisionID)
}

func readYTDecisionBinding(ctx context.Context, q ytRowQuerier, decisionID int64) (YTDecisionBinding, bool, error) {
	var b YTDecisionBinding
	err := scanYTDecisionBinding(q.QueryRow(ctx, `SELECT `+ytDecisionBindingColumns+` FROM yt_decision_bindings WHERE decision_id=$1`,
		strconv.FormatInt(decisionID, 10)), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTDecisionBinding{}, false, nil
	}
	if err != nil {
		return YTDecisionBinding{}, false, err
	}
	return b, true, nil
}

// ytDecisionRow is one decision as the projection reads it: the record (its
// status derived at the pass's time), the answer's channel, its binding and
// whether its decision_create op exists.
type ytDecisionRow struct {
	record  DecisionRecord
	channel string
	binding YTDecisionBinding
	bound   bool
	created bool
}

// readYTDecisionRows reads the projection inputs of ids at now, by id; a
// deleted decision is absent.
func readYTDecisionRows(ctx context.Context, q pgx.Tx, ids []int64, now time.Time) (map[int64]*ytDecisionRow, error) {
	out := make(map[int64]*ytDecisionRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r := &ytDecisionRow{}
		if err := scanDecisionRecord(rows, &r.record, now.UTC()); err != nil {
			rows.Close()
			return nil, err
		}
		out[r.record.ID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The channel of the latest resolution of a structured request, when the
	// row is answered (decision_resolutions, keyed by request id).
	rows, err = q.Query(ctx, `SELECT d.id,
		coalesce((SELECT r.channel FROM decision_resolutions r
			WHERE d.status = 'answered' AND d.source IN ('task_request','linear')
			AND (r.request_id = d.source_ref OR r.revision_request_id = d.source_ref)
			AND r.kind IN ('answered','default_applied')
			ORDER BY r.resolution_id DESC LIMIT 1),''),
		EXISTS(SELECT 1 FROM yt_write_ops o WHERE o.principal = $2 AND o.kind = $3 AND o.idem_key = 'decision-issue:' || d.id::text)
		FROM decision_requests d WHERE d.id = ANY($1::bigint[])`, ids, YTDecisionsPrincipal, ytwrite.KindDecisionCreate)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var channel string
		var created bool
		if err := rows.Scan(&id, &channel, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if r := out[id]; r != nil {
			r.channel, r.created = channel, created
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, strconv.FormatInt(id, 10))
	}
	rows, err = q.Query(ctx, `SELECT `+ytDecisionBindingColumns+` FROM yt_decision_bindings WHERE decision_id = ANY($1::text[])`, keys)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b YTDecisionBinding
		if err := scanYTDecisionBinding(rows, &b); err != nil {
			rows.Close()
			return nil, err
		}
		if r := out[b.DecisionID]; r != nil {
			r.binding, r.bound = b, true
		}
	}
	rows.Close()
	return out, rows.Err()
}

// decisionTextOf is the DEC issue text of a record (the stored question and
// options; the 3514 display rendering is the console's).
func decisionTextOf(d DecisionRecord, channel string) ytwrite.DecisionText {
	x := ytwrite.DecisionText{DecisionID: d.ID, Question: d.Question, Recommended: d.Recommended, DefaultAction: d.DefaultAction,
		Answer: d.Answer, AnsweredBy: d.AnsweredBy, Channel: channel, AnsweredAt: d.AnsweredAt}
	if d.TaskID != nil {
		x.TaskID = *d.TaskID
	}
	if d.Options != nil {
		for _, o := range d.Options.Options {
			x.Options = append(x.Options, ytwrite.DecisionOption{Key: o.Key, Label: o.Label})
		}
	}
	return ytwrite.CanonicalDecision(x)
}

// YTDecisionOutcome is what prepareYTDecisionTx did with one decision.
type YTDecisionOutcome string

const (
	YTDecisionCreatePrepared YTDecisionOutcome = "create_prepared"
	YTDecisionCreateRejected YTDecisionOutcome = "create_rejected"
	YTDecisionCreatePending  YTDecisionOutcome = "create_exists"
	YTDecisionFieldsPrepared YTDecisionOutcome = "fields_prepared"
	YTDecisionUnchanged      YTDecisionOutcome = "unchanged"
	YTDecisionDeleted        YTDecisionOutcome = "deleted"
	YTDecisionOtherTenant    YTDecisionOutcome = "other_tenant"
	YTDecisionInvalid        YTDecisionOutcome = "invalid"
)

type ytDecisionOutcome = YTDecisionOutcome

const (
	ytDecisionCreatePrepared = YTDecisionCreatePrepared
	ytDecisionCreateRejected = YTDecisionCreateRejected
	ytDecisionCreatePending  = YTDecisionCreatePending
	ytDecisionFieldsPrepared = YTDecisionFieldsPrepared
	ytDecisionUnchanged      = YTDecisionUnchanged
	ytDecisionDeleted        = YTDecisionDeleted
	ytDecisionOtherTenant    = YTDecisionOtherTenant
	ytDecisionInvalid        = YTDecisionInvalid
)

// prepareYTDecisionTx decides one decision in tx at now for gate g: a deleted
// decision is skipped; a decision with no binding gets its decision_create
// op unless it already has one (in any state: a rejected create is the
// operator's; the op publishes the binding when it applies); a binding on
// another tenant is skipped; a bound decision prepares its snapshot through
// prepareYTSnapshotTx, which makes no op when the snapshot is the one last
// prepared. A snapshot the protocol cannot store is skipped and logged.
func prepareYTDecisionTx(ctx context.Context, tx pgx.Tx, now time.Time, g ytDecisionsGate, id int64, r *ytDecisionRow) (ytDecisionOutcome, error) {
	switch {
	case r == nil:
		return ytDecisionDeleted, nil
	case !r.bound && r.created:
		return ytDecisionCreatePending, nil
	case !r.bound:
		req := ytwrite.DecisionRequest{Principal: YTDecisionsPrincipal, Actor: YTDecisionsPrincipal, YTProjectID: g.projectID,
			YTProjectShort: g.shortName, Tenant: g.tenant, Decision: decisionTextOf(r.record, r.channel)}
		n, err := ytwrite.DecisionOp(req)
		if err != nil {
			return "", err
		}
		op, created, err := prepareYTOpTx(ctx, tx, now, n)
		if err != nil {
			return "", err
		}
		if !created {
			return ytDecisionCreatePending, nil
		}
		if cerr := ytwrite.CheckDecision(req); cerr != nil {
			if _, err := rejectYTOpTx(ctx, tx, now, op.ID, ytwrite.ReasonInvalidIssue, cerr.Error()); err != nil {
				return "", err
			}
			log.Printf("ytwrite.decision_create_rejected op_id=%d decision_id=%d err=%q", op.ID, id, cerr.Error())
			return ytDecisionCreateRejected, nil
		}
		log.Printf("ytwrite.decision_create_enqueued op_id=%d decision_id=%d", op.ID, id)
		return ytDecisionCreatePrepared, nil
	case r.binding.Tenant != g.tenant:
		return ytDecisionOtherTenant, nil
	}
	n, err := ytwrite.DecisionFieldsOp(ytwrite.DecisionFieldsRequest{Principal: YTDecisionsPrincipal, Actor: YTDecisionsPrincipal,
		Tenant: r.binding.Tenant, IssueID: r.binding.IssueID, ProjectID: r.binding.ProjectID,
		Decision: ytwrite.DecisionFields{DecisionText: decisionTextOf(r.record, r.channel), Status: r.record.Status, Lane: r.record.Lane, DueAt: r.record.DueAt}})
	if errors.Is(err, ytwrite.ErrInvalid) {
		// Bad data must not stop the feed: the cursor would never pass
		// this row.
		log.Printf("ytwrite.decision_fields_skipped decision_id=%d reason=invalid err=%q", id, err.Error())
		return ytDecisionInvalid, nil
	}
	if err != nil {
		return "", err
	}
	op, made, err := prepareYTSnapshotTx(ctx, tx, now, n, n.PayloadSHA)
	if err != nil {
		return "", err
	}
	if !made {
		return ytDecisionUnchanged, nil
	}
	log.Printf("ytwrite.decision_fields_enqueued op_id=%d decision_id=%d issue_id=%q", op.ID, id, r.binding.IssueID)
	return ytDecisionFieldsPrepared, nil
}

// decisionFeedIDs are the decision_requests ids of the feed rows, in order:
// the "record:<id>" decision rows. A request-id row (refs.decision_request)
// is left out: its write also wrote the decision_requests row in the same
// transaction (mirrorTaskDecisionsTx), whose own row is in the feed.
func decisionFeedIDs(rows []FeedRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.Model != "decision" {
			continue
		}
		text, ok := strings.CutPrefix(r.ModelID, decisionRecordFeedPrefix)
		if !ok {
			continue
		}
		if id, err := strconv.ParseInt(text, 10, 64); err == nil && id > 0 && strconv.FormatInt(id, 10) == text {
			ids = append(ids, id)
		}
	}
	return ids
}

// ytDecisionsHandler is the decision_issues consumer's handler for one pass at
// now. Mode off does nothing (the cursor advances). Otherwise each changed
// decision, in sync_id order, is read under its lane lock and goes through
// prepareYTDecisionTx.
func ytDecisionsHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		g, on := s.ytDecisionsNow()
		if !on {
			return nil
		}
		ids := decisionFeedIDs(rows)
		if len(ids) == 0 {
			return nil
		}
		counts := map[ytDecisionOutcome]int{}
		for _, id := range ids {
			// The decision is read under its lane lock, the lock
			// BindYTDecision and a create's Apply publish a binding under:
			// a binding committed before this read is seen, and one after
			// it waits for this pass, whose create op it then refuses to
			// race (ErrYTDecisionOpPending).
			if err := lockYTEntity(ctx, tx, ytwrite.DecisionEntity, strconv.FormatInt(id, 10)); err != nil {
				return err
			}
			decisions, err := readYTDecisionRows(ctx, tx, []int64{id}, now)
			if err != nil {
				return err
			}
			outcome, err := prepareYTDecisionTx(ctx, tx, now, g, id, decisions[id])
			if err != nil {
				return err
			}
			counts[outcome]++
		}
		log.Printf("ytwrite.decisions_feed decisions=%d create_prepared=%d create_rejected=%d create_exists=%d fields_prepared=%d unchanged=%d deleted=%d other_tenant=%d invalid=%d",
			len(ids), counts[ytDecisionCreatePrepared], counts[ytDecisionCreateRejected], counts[ytDecisionCreatePending], counts[ytDecisionFieldsPrepared],
			counts[ytDecisionUnchanged], counts[ytDecisionDeleted], counts[ytDecisionOtherTenant], counts[ytDecisionInvalid])
		return nil
	}
}

// ytDecisionSnapshotTx prepares decision id's snapshot in tx when the mode is
// on and its binding is on the gate's tenant (no op for an unchanged
// snapshot). The caller holds, or prepareYTSnapshotTx takes, the decision's
// lane lock.
func (s *Store) ytDecisionSnapshotTx(ctx context.Context, tx pgx.Tx, id int64, now time.Time) (ytDecisionOutcome, error) {
	g, on := s.ytDecisionsNow()
	if !on {
		return "", nil
	}
	decisions, err := readYTDecisionRows(ctx, tx, []int64{id}, now)
	if err != nil {
		return "", err
	}
	r := decisions[id]
	if r != nil && !r.bound {
		// Not bound (the caller's binding was not published): nothing to
		// snapshot, and no create either.
		return "", nil
	}
	return prepareYTDecisionTx(ctx, tx, now, g, id, r)
}

// ytDecisionConflictTx reads, in Apply's transaction (which holds the
// decision's lane lock), whether publishing issue ytID for op's decision would
// contradict a mapping hk already keeps: the decision bound or mapped to
// another issue, or the issue bound or mapped to another decision. It returns
// the conflict ("" when none) and logs it as ytwrite.binding_conflict.
func ytDecisionConflictTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string) (string, error) {
	id, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || id < 1 || op.EntityKind != ytwrite.DecisionEntity {
		return "", fmt.Errorf("decision_create op %d is not on a decision lane (%s:%s)", op.ID, op.EntityKind, op.EntityID)
	}
	var detail string
	err = tx.QueryRow(ctx, `SELECT coalesce(
		(SELECT 'decision is bound to issue '||issue_id||' on '||tenant||' ('||origin||')' FROM yt_decision_bindings
			WHERE decision_id=$1 AND (tenant<>$2 OR issue_id<>$3)),
		(SELECT 'issue is bound to decision '||decision_id FROM yt_decision_bindings
			WHERE tenant=$2 AND issue_id=$3 AND decision_id<>$1 LIMIT 1),
		(SELECT 'decision is mapped to issue '||yt_id FROM yt_entity_map
			WHERE entity_kind='decision' AND hk_key=$1 AND yt_id<>$3),
		(SELECT 'issue is mapped to decision '||hk_key FROM yt_entity_map
			WHERE entity_kind='decision' AND yt_id=$3 AND hk_key<>$1),
		'')`, op.EntityID, op.Tenant, ytID).Scan(&detail)
	if err != nil {
		return "", err
	}
	if detail != "" {
		log.Printf("ytwrite.binding_conflict decision_id=%d op_id=%d issue_id=%q detail=%q", id, op.ID, ytID, detail)
	}
	return detail, nil
}

// bindYTDecisionApplyTx is the decision_create apply hook's bind: the
// decision's binding (origin op_applied, this op), then its first snapshot,
// which the new binding (no decision change) would not otherwise reach:
// prepared here, behind the op on the decision's lane. The create wrote only
// the summary, the description and the two ids. A binding of the decision to
// this same issue is kept as it is.
func bindYTDecisionApplyTx(s *Store, ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, _ bool) error {
	id, _ := strconv.ParseInt(op.EntityID, 10, 64)
	tag, err := tx.Exec(ctx, `INSERT INTO yt_decision_bindings(decision_id, tenant, issue_id, issue_readable, project_id, origin, op_id, bound_by, bound_at)
		VALUES ($1,$2,$3,'',$4,'op_applied',$5,$6,$7) ON CONFLICT DO NOTHING`,
		op.EntityID, op.Tenant, ytID, op.YTTargetID, op.ID, op.Principal, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		have, found, err := readYTDecisionBinding(ctx, tx, id)
		if err != nil {
			return err
		}
		if !found || have.Tenant != op.Tenant || have.IssueID != ytID {
			// Another decision's binding took the issue after the conflict
			// check: roll the apply back; the next check records it.
			return fmt.Errorf("%w: issue %s on %s was bound to another decision during the apply of op %d", ytwrite.ErrConflictingResult, ytID, op.Tenant, op.ID)
		}
	}
	outcome, err := s.ytDecisionSnapshotTx(ctx, tx, id, now)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.decision_on_bind decision_id=%d outcome=%q", id, outcome)
	return nil
}

var (
	// ErrYTDecisionOpPending: the decision has a decision_create op that is
	// not yet applied or rejected; a backfill binding would race it to a
	// second issue.
	ErrYTDecisionOpPending = errors.New("yt_decision_op_pending")
	// ErrYTDecisionBindingConflict: the decision is bound to another issue,
	// or the issue to another decision.
	ErrYTDecisionBindingConflict = errors.New("yt_decision_binding_conflict")
)

// BindYTDecision publishes a verified binding of an imported DEC issue
// (origin import_verified; `youtrack decisions-backfill`). The caller has
// verified the issue (youtrack.VerifyDecisionIssue). Under the decision's lane
// lock, the lock its ops' claims and applies take: a decision whose
// decision_create op is unresolved is refused (ErrYTDecisionOpPending); a
// binding to another issue, or of the issue to another decision, is refused
// (ErrYTDecisionBindingConflict); the same binding is kept (created false).
// In mode on, the decision's first snapshot is prepared in the same
// transaction, behind any op on its lane.
func (s *Store) BindYTDecision(ctx context.Context, b YTDecisionBinding, now time.Time) (created bool, err error) {
	if b.DecisionID <= 0 || b.Tenant == "" || b.IssueID == "" || b.BoundBy == "" {
		return false, errors.New("a decision binding needs the decision, the tenant, the issue and who bound it")
	}
	key := strconv.FormatInt(b.DecisionID, 10)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, ytwrite.DecisionEntity, key); err != nil {
		return false, err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE kind=$1 AND entity_kind='decision' AND entity_id=$2
		AND state NOT IN ('applied','rejected'))`, ytwrite.KindDecisionCreate, key).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, fmt.Errorf("%w: decision %d", ErrYTDecisionOpPending, b.DecisionID)
	}
	have, found, err := readYTDecisionBinding(ctx, tx, b.DecisionID)
	if err != nil {
		return false, err
	}
	if found {
		if have.Tenant != b.Tenant || have.IssueID != b.IssueID {
			return false, fmt.Errorf("%w: decision %d is bound to issue %s on %s", ErrYTDecisionBindingConflict, b.DecisionID, have.IssueID, have.Tenant)
		}
		return false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO yt_decision_bindings(decision_id, tenant, issue_id, issue_readable, project_id, origin, op_id, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,$5,'import_verified',NULL,$6,$7)`, key, b.Tenant, b.IssueID, b.IssueReadable, b.ProjectID, b.BoundBy, now.UTC())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, fmt.Errorf("%w: issue %s on %s is bound to another decision", ErrYTDecisionBindingConflict, b.IssueID, b.Tenant)
	}
	if err != nil {
		return false, err
	}
	if _, err := s.ytDecisionSnapshotTx(ctx, tx, b.DecisionID, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// YTDecisionCandidate is one decision with no binding, as the backfill sees
// it: PendingOp is true when its decision_create op is unresolved (the op,
// not the backfill, gives it its issue).
type YTDecisionCandidate struct {
	ID        int64 `json:"id"`
	PendingOp bool  `json:"pending_op,omitempty"`
}

// YTDecisionCandidatesMax bounds one page of UnboundYTDecisions.
const YTDecisionCandidatesMax = 1000

// UnboundYTDecisions lists the decisions with no binding and an id above
// afterID, ascending, at most limit (1..YTDecisionCandidatesMax).
func (s *Store) UnboundYTDecisions(ctx context.Context, afterID int64, limit int) ([]YTDecisionCandidate, error) {
	if afterID < 0 || limit < 1 || limit > YTDecisionCandidatesMax {
		return nil, errors.New("invalid decision candidate page")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id, EXISTS(SELECT 1 FROM yt_write_ops o WHERE o.kind=$3 AND o.entity_kind='decision'
			AND o.entity_id=d.id::text AND o.state NOT IN ('applied','rejected'))
		FROM decision_requests d
		WHERE d.id > $1 AND NOT EXISTS (SELECT 1 FROM yt_decision_bindings b WHERE b.decision_id = d.id::text)
		ORDER BY d.id LIMIT $2`, afterID, limit, ytwrite.KindDecisionCreate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTDecisionCandidate{}
	for rows.Next() {
		var c YTDecisionCandidate
		if err := rows.Scan(&c.ID, &c.PendingOp); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PrepareYTDecision prepares a decision's op in its own transaction under its
// lane lock: a decision_create op when unbound (and missing its op); a
// decision_fields snapshot op when bound (and its snapshot sha differs).
// Mode off or other tenant returns ("", nil).
func (s *Store) PrepareYTDecision(ctx context.Context, id int64, now time.Time) (YTDecisionOutcome, error) {
	g, on := s.ytDecisionsNow()
	if !on {
		return "", nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, ytwrite.DecisionEntity, strconv.FormatInt(id, 10)); err != nil {
		return "", err
	}
	decisions, err := readYTDecisionRows(ctx, tx, []int64{id}, now)
	if err != nil {
		return "", err
	}
	outcome, err := prepareYTDecisionTx(ctx, tx, now, g, id, decisions[id])
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcome, nil
}

// BoundYTDecisions lists, in decision id order, the ids of decisions bound on
// tenant with an id greater than afterID, at most limit (1..YTDecisionCandidatesMax).
func (s *Store) BoundYTDecisions(ctx context.Context, tenant string, afterID int64, limit int) ([]int64, error) {
	if tenant == "" || afterID < 0 || limit < 1 || limit > YTDecisionCandidatesMax {
		return nil, errors.New("invalid bound decision page")
	}
	rows, err := s.pool.Query(ctx, `SELECT decision_id::bigint FROM yt_decision_bindings
		WHERE tenant = $1 AND decision_id::bigint > $2
		ORDER BY decision_id::bigint LIMIT $3`, tenant, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

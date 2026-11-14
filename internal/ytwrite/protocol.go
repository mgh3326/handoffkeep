package ytwrite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Result is the one structured answer every caller (HTTP, MCP, CLI) gets for a
// write, whatever happened. Pending is not success: a caller that needs the
// effect waits and calls Lookup with OpID, after RetryAfter.
type Result struct {
	OpID   int64
	Status Status
	// State is the op's lifecycle state, for status pages and logs.
	State State
	// Reason is the cause code of a pending or rejected answer ("" when
	// applied).
	Reason string
	// RetryAfter is when a pending caller should look again; 0 for a terminal
	// answer.
	RetryAfter time.Duration
	// YTID is the created YouTrack id, set only when applied.
	YTID string
	// Replayed is true when the key already named a recorded op.
	Replayed bool
}

// Config wires a Protocol. Zero durations take the defaults noted.
type Config struct {
	Ledger Ledger
	Remote Remote
	// Now returns the current time (default time.Now().UTC()). Every
	// timestamp the ledger stores comes from it.
	Now func() time.Time
	// DispatchTimeout bounds one remote call and one evidence search (30s). A
	// caller's cancellation does not cancel either: the write is durable and
	// its reconciliation must finish.
	DispatchTimeout time.Duration
	// SettleTimeout bounds recording what one remote call returned (10s). The
	// caller's cancellation does not cut it short, but a settlement that
	// cannot finish in time (a stopping server, a locked row) is abandoned:
	// the op stays dispatched, which stale recovery turns into remote_unknown
	// and the marker search resolves. It is never recorded as applied without
	// the ledger's own commit.
	SettleTimeout time.Duration
	// StaleAfter is how long a dispatched op may stay in flight before it is
	// treated as lost and becomes remote_unknown (twice DispatchTimeout).
	StaleAfter time.Duration
	// RateLimitWait is the wait after a 429 without Retry-After (5s);
	// MaxRetryAfter caps a Retry-After the remote names (RetryAfterCap, 30
	// days): a sanity bound for absurd values, not a shortening of real
	// ones. The server's requested wait is honoured to the second below it.
	RateLimitWait time.Duration
	MaxRetryAfter time.Duration
	// AuthWait is the wait after a 401, which needs a repaired token (5m).
	AuthWait time.Duration
	// ReconcileBase and ReconcileMax bound the initial doubling wait between
	// evidence searches for one remote_unknown op (5s, 5m). After 50 searches,
	// the protocol retries hourly, then daily after 24 more evidence searches.
	ReconcileBase time.Duration
	ReconcileMax  time.Duration
	// BlockedWait is the retry hint for an op queued behind an earlier one (5s).
	BlockedWait time.Duration
	// PageSize is the comment page requested when searching for evidence (100).
	PageSize int
}

// RetryAfterCap is the longest Retry-After the protocol records. A longer
// value is not a real rate limit; it is recorded as this cap, never dropped.
const RetryAfterCap = 30 * 24 * time.Hour

// maxDrivePasses bounds the passes of one Drive call.
const maxDrivePasses = 16

// Protocol runs the write protocol over a Ledger and a Remote.
type Protocol struct {
	Config
	// Kinds is the registry send and reconcile dispatch through, by op kind.
	// New fills it with every registered kind; an op of any other kind is
	// rejected unsupported_kind. Tests may add a kind before the first call.
	Kinds map[string]Kind
}

// New fills the defaults and returns a Protocol.
func New(cfg Config) (*Protocol, error) {
	if cfg.Ledger == nil || cfg.Remote == nil {
		return nil, errors.New("ytwrite: ledger and remote are required")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.DispatchTimeout, 30*time.Second)
	def(&cfg.StaleAfter, 2*cfg.DispatchTimeout)
	if cfg.StaleAfter <= cfg.DispatchTimeout {
		return nil, fmt.Errorf("ytwrite: StaleAfter %s must exceed DispatchTimeout %s", cfg.StaleAfter, cfg.DispatchTimeout)
	}
	def(&cfg.SettleTimeout, 10*time.Second)
	def(&cfg.RateLimitWait, 5*time.Second)
	def(&cfg.MaxRetryAfter, RetryAfterCap)
	def(&cfg.AuthWait, 5*time.Minute)
	def(&cfg.ReconcileBase, 5*time.Second)
	def(&cfg.ReconcileMax, 5*time.Minute)
	def(&cfg.BlockedWait, 5*time.Second)
	if cfg.PageSize <= 0 {
		cfg.PageSize = 100
	}
	kinds := make(map[string]Kind, len(kindBuilders))
	for name, build := range kindBuilders {
		kinds[name] = build(cfg)
	}
	return &Protocol{Config: cfg, Kinds: kinds}, nil
}

func (p *Protocol) now() time.Time { return p.Now().UTC() }

// CommentRequest is one comment to append to the issue mapped to a task.
type CommentRequest struct {
	// Principal is the authenticated caller; keys are scoped to it.
	Principal string
	// Key is the caller's idempotency key. Empty generates one, which makes
	// the call undeduplicable: the caller must keep the returned OpID.
	Key string
	// Actor is who the comment is attributed to in its provenance line.
	Actor string
	// TaskID is the hk task (the ordering lane); YTIssueID is its mapped
	// YouTrack issue id, resolved by the caller.
	TaskID    int64
	YTIssueID string
	// Tenant is the YouTrack host YTIssueID belongs to.
	Tenant string
	Body   string
}

// CheckComment explains why a comment request is refused before anything is
// stored. An empty Key is accepted (AppendComment generates one).
func CheckComment(req CommentRequest) error { return checkCommentRequest(req, textBodyLimit) }

// CheckCommentContent explains why a comment's author or body can never be
// sent, whatever issue it targets: the part of CheckComment that depends on
// the comment alone.
func CheckCommentContent(actor, body string) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	if err := checkBodyMax(body, textBodyLimit); err != nil {
		return err
	}
	return checkCommentFits(actor, body)
}

// checkCommentFits refuses a comment whose sent text would be over YouTrack's
// bound under some op id. The body alone may be within the bound: the
// provenance line and the marker are added to it when the text is rendered.
func checkCommentFits(actor, body string) error {
	if !commentFitsAnyOpID(actor, body) {
		return fmt.Errorf("%w: rendered comment is over %d characters (YouTrack's text bound)", ErrInvalid, TextMaxChars)
	}
	return nil
}

// checkCommentRequest is CheckComment with the body size rule given.
func checkCommentRequest(req CommentRequest, lim bodyLimit) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.TaskID <= 0 {
		return fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.YTIssueID) {
		return fmt.Errorf("%w: youtrack issue id is empty or malformed", ErrInvalid)
	}
	if err := checkBodyMax(req.Body, lim); err != nil {
		return err
	}
	if err := checkCommentFits(req.Actor, req.Body); err != nil {
		return err
	}
	if req.Key != "" && !keyRE.MatchString(req.Key) {
		return fmt.Errorf("%w: idempotency key is malformed", ErrInvalid)
	}
	return nil
}

// CommentOp is the op a comment request stores: the immutable payload, its
// hash, the entity lane ("task", decimal id) and the target. It does not
// validate the rest of the request: a store that must record a refused
// comment as a rejected op builds it here and rejects it in the same
// transaction. req.Key must be set.
func CommentOp(req CommentRequest) (NewOp, error) {
	if req.Key == "" {
		return NewOp{}, fmt.Errorf("%w: idempotency key is empty", ErrInvalid)
	}
	payload, err := json.Marshal(CommentPayload{Body: req.Body})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.TaskID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindCommentAppend,
		Key:        req.Key,
		PayloadSHA: payloadSHA(KindCommentAppend, "task", entityID, req.YTIssueID, req.Tenant, req.Actor, req.Body),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.YTIssueID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// AppendComment records one comment write durably and tries to dispatch it
// once. It returns an error only before the op row is durable (invalid request,
// key conflict, ledger unavailable); after that the answer is a Result.
// hk's own comment path does not call it: the store prepares the op inside the
// comment's transaction and the worker's Drive sends it.
func (p *Protocol) AppendComment(ctx context.Context, req CommentRequest) (Result, error) {
	if err := CheckComment(req); err != nil {
		return Result{}, err
	}
	if req.Key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Result{}, fmt.Errorf("ytwrite: generate key: %w", err)
		}
		req.Key = "auto-" + hex.EncodeToString(b[:])
	}
	n, err := CommentOp(req)
	if err != nil {
		return Result{}, err
	}
	op, created, err := p.Ledger.Prepare(ctx, p.now(), n)
	if err != nil {
		return Result{}, err
	}
	res := p.advance(ctx, op)
	res.Replayed = !created
	return res, nil
}

// Lookup returns the current Result of an op the principal owns.
func (p *Protocol) Lookup(ctx context.Context, principal string, id int64) (Result, error) {
	op, err := p.Ledger.Get(ctx, principal, id)
	if err != nil {
		return Result{}, err
	}
	return p.result(op, false), nil
}

// result maps an op to the caller's answer. blocked marks an op that was
// refused its dispatch because an earlier op on its entity is unresolved.
func (p *Protocol) result(op Op, blocked bool) Result {
	r := Result{OpID: op.ID, State: op.State, Reason: op.Reason, Status: StatusOf(op.State)}
	switch op.State {
	case StateApplied:
		r.YTID, r.Reason = op.YTResultID, ""
	case StateRejected:
	default:
		wait := time.Duration(0)
		if d := op.NextAttemptAt.Sub(p.now()); d > 0 {
			wait = d
		}
		switch {
		case blocked:
			r.Reason = ReasonWaitingForEarlier
			wait = max(wait, p.BlockedWait)
		case op.State == StateDispatched:
			r.Reason = ReasonDispatched
			wait = max(wait, p.BlockedWait)
		case op.State == StateRemoteUnknown && r.Reason == "":
			r.Reason = ReasonRemoteUnknown
		}
		r.RetryAfter = wait
	}
	return r
}

// advance tries one dispatch of a prepared op and returns its answer. Other
// states are answered from the record.
func (p *Protocol) advance(ctx context.Context, op Op) Result {
	if op.State != StatePrepared {
		return p.result(op, false)
	}
	cur, outcome, err := p.Ledger.Claim(ctx, p.now(), op.ID)
	if err != nil {
		// The op is durable; whether the claim committed is unknown, and
		// nothing was sent. Report it as the record last showed it.
		log.Printf("ytwrite.claim_failed op_id=%d err=%q", op.ID, err.Error())
		return p.result(op, false)
	}
	switch outcome {
	case ClaimClaimed:
		return p.send(ctx, cur)
	case ClaimBlocked:
		return p.result(cur, true)
	case ClaimHeld:
		r := p.result(cur, false)
		r.Reason = ReasonHeld
		r.RetryAfter = max(r.RetryAfter, p.BlockedWait)
		return r
	default:
		return p.result(cur, false)
	}
}

func (p *Protocol) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), p.DispatchTimeout)
}

// settling is the context one settlement runs on: not canceled with the
// caller, but bounded by SettleTimeout.
func (p *Protocol) settling(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), p.SettleTimeout)
}

var resultIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// send makes the one remote attempt for a claimed op and records what is
// known about it.
func (p *Protocol) send(ctx context.Context, op Op) Result {
	// From here the write is in flight and durable: the caller going away must
	// not stop the call or its recording, but each is bounded (DispatchTimeout,
	// SettleTimeout), so a stopping worker returns and an unrecorded dispatch
	// is left for stale recovery.
	kind, ok := p.Kinds[op.Kind]
	if !ok {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, p.reject(sctx, op, ReasonUnsupportedKind, op.Kind))
	}
	if pk, ok := kind.(PlannedKind); ok {
		return p.sendPlanned(ctx, op, pk)
	}
	text, err := kind.Render(op)
	if err != nil {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, p.reject(sctx, op, ReasonUnsupportedKind, err.Error()))
	}
	cctx, cancelCall := p.detached(ctx)
	id, err := kind.Send(cctx, p.Remote, op, text)
	cancelCall()
	ctx, cancel := p.settling(ctx)
	defer cancel()
	now := p.now()
	var refused *RefusedError
	var notSent *NotSentError
	var adopted *AdoptedError
	switch {
	case err == nil && resultIDRE.MatchString(id):
		return p.record(op, p.apply(ctx, kind, op, id, false))
	case errors.As(err, &adopted) && resultIDRE.MatchString(adopted.ID):
		// The decision is recorded before it is applied, so an apply that
		// is lost leaves reconcile the proof that nothing was sent (hk
		// 4570). A record that fails leaves the operator's resolve, as
		// before; the apply is still tried.
		if rerr := p.Ledger.RecordAdoption(ctx, now, op.ID, op.Attempts, adopted.ID); rerr != nil {
			log.Printf("ytwrite.adoption_record_failed op_id=%d yt_id=%q err=%q", op.ID, adopted.ID, rerr.Error())
		}
		return p.record(op, p.apply(ctx, kind, op, adopted.ID, true))
	case adopted != nil:
		// Nothing was written; an id the ledger cannot hold is no adoption.
		return p.record(op, p.reject(ctx, op, ReasonNoID, "adopted id is empty or malformed"))
	case err == nil:
		// 2xx without a usable id: the write may exist.
		return p.record(op, p.markUnknown(ctx, op, ReasonNoID, "2xx reply without a valid id"))
	case errors.As(err, &refused):
		// The kind refused before it wrote anything.
		return p.record(op, p.reject(ctx, op, refused.Reason, refused.Detail))
	case errors.As(err, &notSent):
		return p.record(op, p.notSent(ctx, op, notSent))
	default:
		return p.record(op, p.writeFailed(ctx, op, err))
	}
}

// writeFailed records a write request that failed before anything it sent is
// known to have taken effect: 429 and 401 were not processed (prepared, after
// the wait), another 4xx is final, and a 5xx, 408 or transport failure may
// have taken effect (remote_unknown).
func (p *Protocol) writeFailed(ctx context.Context, op Op, err error) settled {
	now := p.now()
	var re *RemoteError
	switch {
	case errors.As(err, &re):
		switch {
		case re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			wait = min(wait, p.MaxRetryAfter)
			return p.deferOp(ctx, op, now.Add(wait), ReasonRateLimited, re.Error())
		case re.Status == http.StatusUnauthorized:
			return p.deferOp(ctx, op, now.Add(p.AuthWait), ReasonUnauthorized, re.Error())
		case re.Status >= 500 || re.Status == http.StatusRequestTimeout:
			return p.markUnknown(ctx, op, ReasonServerError, re.Error())
		case re.Status >= 400 && re.Status < 500:
			return p.reject(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error())
		}
		return p.markUnknown(ctx, op, ReasonTransport, re.Error())
	default:
		return p.markUnknown(ctx, op, ReasonTransport, err.Error())
	}
}

// settled is the outcome of one ledger step after a remote answer: the op as
// recorded, or the error that kept it from being recorded.
type settled struct {
	op  Op
	err error
}

// record turns a settled step into the caller's answer. A step the ledger
// could not record leaves the op dispatched; the op is still durable and the
// stale-dispatch recovery makes it remote_unknown, so the caller hears pending.
func (p *Protocol) record(before Op, s settled) Result {
	if s.err != nil {
		log.Printf("ytwrite.outcome_record_failed op_id=%d err=%q", before.ID, s.err.Error())
		before.Reason = ReasonOutcomeUnrecorded
		return Result{OpID: before.ID, Status: StatusPending, State: before.State, Reason: ReasonOutcomeUnrecorded, RetryAfter: p.StaleAfter}
	}
	logTransition(s.op)
	return p.result(s.op, false)
}

// logTransition is the one log event per recorded state change: the worker's
// metrics are these lines.
func logTransition(op Op) {
	log.Printf("ytwrite.op_state op_id=%d entity_kind=%s entity_id=%q state=%s reason=%q attempts=%d reconciles=%d", op.ID, op.EntityKind, op.EntityID, op.State, op.Reason, op.Attempts, op.Reconciles)
}

// notSent records a read that failed before the write left: nothing was
// written, so the op is not uncertain. A 429 or 401 waits as a refused write
// would, another 4xx is final, and anything else (5xx, transport) is tried
// again after the capped dispatch retry backoff.
func (p *Protocol) notSent(ctx context.Context, op Op, ns *NotSentError) settled {
	now := p.now()
	var re *RemoteError
	if errors.As(ns.Err, &re) {
		switch {
		case re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			return p.deferOp(ctx, op, now.Add(min(wait, p.MaxRetryAfter)), ReasonRateLimited, re.Error())
		case re.Status == http.StatusUnauthorized:
			return p.deferOp(ctx, op, now.Add(p.AuthWait), ReasonUnauthorized, re.Error())
		case re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout:
			return p.reject(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error())
		}
	}
	return p.deferOp(ctx, op, now.Add(p.retryDelay(max(op.Attempts-1, 0))), ReasonNotSent, ns.Error())
}

func (p *Protocol) apply(ctx context.Context, kind Kind, op Op, ytID string, adopted bool) settled {
	maps := kind.MapRows(op, ytID)
	for i := range maps {
		maps[i].Adopted = adopted
	}
	got, err := p.Ledger.Apply(ctx, p.now(), op.ID, ytID, maps)
	if errors.Is(err, ErrConflictingResult) {
		log.Printf("ytwrite.apply_conflict_failed op_id=%d yt_id=%q", op.ID, ytID)
	}
	return settled{got, err}
}

func commentMap(op Op, ytID string) MapRow {
	return MapRow{EntityKind: "comment", HKKey: "op:" + strconv.FormatInt(op.ID, 10), YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}
}

func (p *Protocol) reject(ctx context.Context, op Op, reason, detail string) settled {
	got, err := p.Ledger.Reject(ctx, p.now(), op.ID, reason, detail)
	return settled{got, err}
}

// deferOp is Defer with the answer wrapped.
func (p *Protocol) deferOp(ctx context.Context, op Op, next time.Time, reason, detail string) settled {
	got, err := p.Ledger.Defer(ctx, p.now(), op.ID, next, reason, detail)
	return settled{got, err}
}

func (p *Protocol) markUnknown(ctx context.Context, op Op, reason, detail string) settled {
	now := p.now()
	got, err := p.Ledger.MarkUnknown(ctx, now, op.ID, now.Add(p.reconcileDelay(0)), reason, detail)
	return settled{got, err}
}

// reconcileDelay is the wait after the n-th evidence search of an op.
func (p *Protocol) reconcileDelay(n int) time.Duration {
	if n >= 74 {
		return 24 * time.Hour
	}
	if n >= 50 {
		return time.Hour
	}
	return p.retryDelay(n)
}

// retryDelay is the capped exponential wait used for dispatch retries and as
// the initial wait before evidence-search thresholds take effect.
func (p *Protocol) retryDelay(n int) time.Duration {
	d := p.ReconcileBase
	for i := 0; i < n && d < p.ReconcileMax; i++ {
		d *= 2
	}
	return min(d, p.ReconcileMax)
}

// DriveStats counts what one Drive pass did.
type DriveStats struct {
	Recovered  int // dispatched ops found lost and made remote_unknown
	Visited    int // prepared ops offered a dispatch (some stay queued)
	Reconciled int // remote_unknown ops searched for evidence
	Applied    int // ops that ended applied during this pass
}

// Drive is one worker pass: dispatched ops that were lost become remote_unknown,
// then every due op, oldest first, is dispatched (prepared) or searched for
// evidence (remote_unknown). Run it on a timer and after a restart. Errors on
// one op do not stop the pass; they are joined.
func (p *Protocol) Drive(ctx context.Context, limit int) (DriveStats, error) {
	var st DriveStats
	var errs []error
	now := p.now()
	stale, err := p.Ledger.Stale(ctx, now.Add(-p.StaleAfter), limit)
	if err != nil {
		return st, err
	}
	for _, op := range stale {
		if ctx.Err() != nil {
			break
		}
		recovered, err := p.Ledger.RecoverStale(ctx, op.ID, op.Attempts, now.Add(-p.StaleAfter), now, "dispatch did not finish before "+p.StaleAfter.String())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if recovered {
			st.Recovered++
			log.Printf("ytwrite.op_state op_id=%d entity_kind=%s entity_id=%q state=%s reason=%q attempts=%d reconciles=%d", op.ID, op.EntityKind, op.EntityID, StateRemoteUnknown, ReasonDispatcherLost, op.Attempts, op.Reconciles)
		}
	}
	// An op that resolves releases the writes queued behind it on its entity,
	// and Due does not list those while it is unresolved, so go round again
	// while a pass resolves something.
	for pass := 0; pass < maxDrivePasses; pass++ {
		if ctx.Err() != nil {
			break
		}
		due, err := p.Ledger.Due(ctx, p.now(), limit)
		if err != nil {
			return st, errors.Join(append(errs, err)...)
		}
		resolved := false
		for _, op := range due {
			// A stopping worker claims nothing new; what is in flight finishes
			// on its detached context and the rest stays durable.
			if ctx.Err() != nil {
				break
			}
			switch op.State {
			case StatePrepared:
				st.Visited++
				if r := p.advance(ctx, op); r.Status != StatusPending {
					resolved = true
					if r.Status == StatusApplied {
						st.Applied++
					}
				}
			case StateRemoteUnknown:
				st.Reconciled++
				applied, err := p.reconcile(ctx, op)
				if err != nil {
					errs = append(errs, err)
				}
				if applied {
					st.Applied++
					resolved = true
				}
			}
		}
		if !resolved {
			break
		}
	}
	return st, errors.Join(errs...)
}

// reconcile searches the op's target for its exact rendered text through the
// op's kind. One exact match is positive evidence and applies the op; one
// match the kind reports as the thing Send would have adopted
// (Evidence.Adopted) is applied as adopted only when the op's last dispatch
// recorded that it adopted that same thing (Ledger.RecordAdoption, hk 4570):
// then it sent nothing. Without that record nothing bounds when a create the
// op's dispatch may have sent lands, and an applied op is never searched
// again, so a late create would be a second issue no one sees (review of PR
// #383). It stays remote_unknown / adoption_unconfirmed, the candidate named
// in the detail, for an operator's resolve --adopted. Anything else leaves it
// remote_unknown: no match is not proof
// the write did not happen, so there is no automatic re-create. The decision
// is the same for every kind; a kind only reports what it found (Evidence).
func (p *Protocol) reconcile(ctx context.Context, op Op) (applied bool, err error) {
	var wait time.Duration
	touch := func(reason, detail string) (bool, error) {
		return false, p.touchUnknown(ctx, op, wait, reason, detail)
	}
	kind, ok := p.Kinds[op.Kind]
	if !ok {
		return touch(ReasonReconcileFailed, "unsupported kind "+op.Kind)
	}
	if pk, ok := kind.(PlannedKind); ok {
		return p.reconcilePlanned(ctx, op, pk)
	}
	want, rerr := kind.Render(op)
	if rerr != nil {
		return touch(ReasonReconcileFailed, rerr.Error())
	}
	cctx, cancel := p.detached(ctx)
	defer cancel()
	ev, lerr := kind.Evidence(cctx, p.Remote, op, want)
	if lerr != nil {
		wait = ev.RetryAfter
		return touch(ReasonReconcileFailed, lerr.Error())
	}
	switch {
	case len(ev.Matches) == 1 && ev.Deleted == 0 && ev.Mismatched == 0 && resultIDRE.MatchString(ev.Matches[0]):
		if ev.Adopted {
			a, found, aerr := p.Ledger.Adoption(ctx, op.ID)
			if aerr != nil {
				return touch(ReasonReconcileFailed, aerr.Error())
			}
			if found && a.YTID == ev.Matches[0] && a.Attempts == op.Attempts {
				// This op's last dispatch adopted this issue and sent
				// nothing (RecordAdoption): no create of its can land.
				s := p.apply(ctx, kind, op, a.YTID, true)
				if s.err == nil {
					logTransition(s.op)
				}
				return s.err == nil, s.err
			}
			return touch(ReasonAdoptionUnconfirmed, ev.Matches[0]+" is the task's unmarked issue, the one Send adopts; a create this op sent may still land, so an operator resolves it (--applied "+ev.Matches[0]+" --adopted)")
		}
		s := p.apply(ctx, kind, op, ev.Matches[0], false)
		if s.err == nil {
			logTransition(s.op)
		}
		return s.err == nil, s.err
	case len(ev.Matches) > 1:
		return touch(ReasonMultipleMatches, fmt.Sprintf("%d candidates carry the marker", len(ev.Matches)))
	case ev.Deleted > 0:
		return touch(ReasonMarkerDeleted, "the marked candidate was deleted")
	case ev.Mismatched > 0:
		return touch(ReasonMarkerMismatch, fmt.Sprintf("%d candidates disagree with the rendered text", ev.Mismatched))
	}
	return touch(ReasonNoEvidence, "no candidate carries the marker")
}

// touchUnknown records one evidence search (or recovery pass) of op that did
// not resolve it: due again after the reconcile backoff, or after wait (a
// 429's Retry-After, a 401's AuthWait) when that is longer.
func (p *Protocol) touchUnknown(ctx context.Context, op Op, wait time.Duration, reason, detail string) error {
	now := p.now()
	delay := max(p.reconcileDelay(op.Reconciles+1), min(wait, p.MaxRetryAfter))
	if op.Reconciles >= 49 {
		log.Printf("ytwrite.reconcile_warning op_id=%d reconciles=%d next_after=%q reason=%q", op.ID, op.Reconciles+1, delay, reason)
	}
	got, err := p.Ledger.TouchUnknown(ctx, now, op.ID, now.Add(delay), reason, detail)
	if err == nil {
		logTransition(got)
	}
	return err
}

// Resolution is an operator's decision on a remote_unknown op after looking at
// YouTrack themselves.
type Resolution struct {
	// Applied says the comment exists, and YTID names it. False says it does
	// not exist and the op is closed as rejected.
	Applied bool
	YTID    string
	// Adopted says whether YTID existed before the op (it adopted an issue
	// made elsewhere: true) or was created by it (false); only with Applied.
	// An adopted issue already holds the task's history, so its binding
	// starts after the comments the task had when the op was prepared (the
	// store's yt_issue_comment_bounds, hk 4301); a created one starts at 0.
	// nil is "not
	// stated", which a kind whose binding depends on it refuses
	// (issue_create), so an older client cannot pick a cutoff by omission.
	Adopted *bool
	By      string
	Note    string
}

// Resolve closes a remote_unknown op on an operator's word. It is the only way
// out of remote_unknown besides positive evidence, and it never resends.
func (p *Protocol) Resolve(ctx context.Context, id int64, r Resolution) (Result, error) {
	op, err := p.Ledger.ByID(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if op.State != StateRemoteUnknown {
		return Result{}, fmt.Errorf("%w: op %d is %s, not remote_unknown", ErrState, id, op.State)
	}
	if err := checkActor(r.By); err != nil {
		return Result{}, err
	}
	if r.Adopted != nil && !r.Applied {
		return Result{}, fmt.Errorf("%w: adopted goes with applied only", ErrInvalid)
	}
	var got Op
	if r.Applied {
		if !resultIDRE.MatchString(r.YTID) {
			return Result{}, fmt.Errorf("%w: youtrack id is empty or malformed", ErrInvalid)
		}
		kind, ok := p.kindFor(op.Kind)
		if !ok {
			return Result{}, fmt.Errorf("%w: op %d has unsupported kind %q", ErrInvalid, id, op.Kind)
		}
		if pk, ok := kind.(provenanceKind); ok && pk.needsProvenance() && r.Adopted == nil {
			return Result{}, fmt.Errorf("%w: op %d is a %s: say whether the YouTrack id was created by it or adopted (adopted true or false)", ErrInvalid, id, op.Kind)
		}
		// The ledger did not record the provenance; the operator states it.
		maps := kind.MapRows(op, r.YTID)
		for i := range maps {
			maps[i].Adopted = r.Adopted != nil && *r.Adopted
		}
		got, err = p.Ledger.Apply(ctx, p.now(), id, r.YTID, maps)
	} else {
		got, err = p.Ledger.Reject(ctx, p.now(), id, ReasonOperatorNotDone, "by "+r.By+": "+r.Note)
	}
	if err != nil {
		return Result{}, err
	}
	return p.result(got, false), nil
}

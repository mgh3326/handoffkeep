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
	"strings"
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
	// StaleAfter is how long a dispatched op may stay in flight before it is
	// treated as lost and becomes remote_unknown (twice DispatchTimeout).
	StaleAfter time.Duration
	// RateLimitWait is the wait after a 429 without Retry-After (5s);
	// MaxRetryAfter caps a Retry-After the remote names (1h). The old client's
	// 5s cap is gone: the server's requested wait is honoured.
	RateLimitWait time.Duration
	MaxRetryAfter time.Duration
	// AuthWait is the wait after a 401, which needs a repaired token (5m).
	AuthWait time.Duration
	// ReconcileBase and ReconcileMax bound the doubling wait between evidence
	// searches for one remote_unknown op (5s, 5m).
	ReconcileBase time.Duration
	ReconcileMax  time.Duration
	// BlockedWait is the retry hint for an op queued behind an earlier one (5s).
	BlockedWait time.Duration
	// PageSize is the comment page requested when searching for evidence (100).
	PageSize int
}

// maxDrivePasses bounds the passes of one Drive call.
const maxDrivePasses = 16

// Protocol runs the write protocol over a Ledger and a Remote.
type Protocol struct {
	Config
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
	def(&cfg.RateLimitWait, 5*time.Second)
	def(&cfg.MaxRetryAfter, time.Hour)
	def(&cfg.AuthWait, 5*time.Minute)
	def(&cfg.ReconcileBase, 5*time.Second)
	def(&cfg.ReconcileMax, 5*time.Minute)
	def(&cfg.BlockedWait, 5*time.Second)
	if cfg.PageSize <= 0 {
		cfg.PageSize = 100
	}
	return &Protocol{Config: cfg}, nil
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
	Body      string
}

// AppendComment records one comment write durably and tries to dispatch it
// once. It returns an error only before the op row is durable (invalid request,
// key conflict, ledger unavailable); after that the answer is a Result.
func (p *Protocol) AppendComment(ctx context.Context, req CommentRequest) (Result, error) {
	if !principalRE.MatchString(req.Principal) {
		return Result{}, fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return Result{}, err
	}
	if req.TaskID <= 0 {
		return Result{}, fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.YTIssueID) {
		return Result{}, fmt.Errorf("%w: youtrack issue id is empty or malformed", ErrInvalid)
	}
	if err := checkBody(req.Body); err != nil {
		return Result{}, err
	}
	key := req.Key
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Result{}, fmt.Errorf("ytwrite: generate key: %w", err)
		}
		key = "auto-" + hex.EncodeToString(b[:])
	} else if !keyRE.MatchString(key) {
		return Result{}, fmt.Errorf("%w: idempotency key is malformed", ErrInvalid)
	}
	payload, err := json.Marshal(CommentPayload{Body: req.Body})
	if err != nil {
		return Result{}, err
	}
	entityID := strconv.FormatInt(req.TaskID, 10)
	op, created, err := p.Ledger.Prepare(ctx, p.now(), NewOp{
		Principal:  req.Principal,
		Kind:       KindCommentAppend,
		Key:        key,
		PayloadSHA: payloadSHA(KindCommentAppend, "task", entityID, req.YTIssueID, req.Actor, req.Body),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.YTIssueID,
		Actor:      req.Actor,
	})
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
	r := Result{OpID: op.ID, State: op.State, Reason: op.Reason}
	switch op.State {
	case StateApplied:
		r.Status, r.YTID, r.Reason = StatusApplied, op.YTResultID, ""
	case StateRejected:
		r.Status = StatusRejected
	default:
		r.Status = StatusPending
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
	default:
		return p.result(cur, false)
	}
}

func (p *Protocol) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), p.DispatchTimeout)
}

var resultIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// send makes the one remote attempt for a claimed op and records what is
// known about it.
func (p *Protocol) send(ctx context.Context, op Op) Result {
	// From here the write is in flight and durable: the caller going away must
	// not stop it from being recorded.
	ctx = context.WithoutCancel(ctx)
	if op.Kind != KindCommentAppend {
		return p.record(op, p.reject(ctx, op, ReasonUnsupportedKind, op.Kind))
	}
	text, err := RenderComment(op)
	if err != nil {
		return p.record(op, p.reject(ctx, op, ReasonUnsupportedKind, err.Error()))
	}
	cctx, cancel := p.detached(ctx)
	defer cancel()
	id, err := p.Remote.CreateComment(cctx, op.YTTargetID, text)
	now := p.now()
	var re *RemoteError
	switch {
	case err == nil && resultIDRE.MatchString(id):
		return p.record(op, p.apply(ctx, op, id))
	case err == nil:
		// 2xx without a usable id: the comment may exist.
		return p.record(op, p.markUnknown(ctx, op, ReasonNoID, "2xx reply without a valid comment id"))
	case errors.As(err, &re):
		switch {
		case re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			wait = min(wait, p.MaxRetryAfter)
			return p.record(op, p.deferOp(ctx, op, now.Add(wait), ReasonRateLimited, re.Error()))
		case re.Status == http.StatusUnauthorized:
			return p.record(op, p.deferOp(ctx, op, now.Add(p.AuthWait), ReasonUnauthorized, re.Error()))
		case re.Status >= 500 || re.Status == http.StatusRequestTimeout:
			return p.record(op, p.markUnknown(ctx, op, ReasonServerError, re.Error()))
		case re.Status >= 400 && re.Status < 500:
			return p.record(op, p.reject(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error()))
		}
		return p.record(op, p.markUnknown(ctx, op, ReasonTransport, re.Error()))
	default:
		return p.record(op, p.markUnknown(ctx, op, ReasonTransport, err.Error()))
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
	return p.result(s.op, false)
}

func (p *Protocol) apply(ctx context.Context, op Op, ytID string) settled {
	got, err := p.Ledger.Apply(ctx, p.now(), op.ID, ytID, []MapRow{commentMap(op, ytID)})
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

// reconcileDelay is the wait before the n-th evidence search of an op.
func (p *Protocol) reconcileDelay(n int) time.Duration {
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
		recovered, err := p.Ledger.RecoverStale(ctx, op.ID, op.Attempts, now.Add(-p.StaleAfter), now, "dispatch did not finish before "+p.StaleAfter.String())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if recovered {
			st.Recovered++
		}
	}
	// An op that resolves releases the writes queued behind it on its entity,
	// and Due does not list those while it is unresolved, so go round again
	// while a pass resolves something.
	for pass := 0; pass < maxDrivePasses; pass++ {
		due, err := p.Ledger.Due(ctx, p.now(), limit)
		if err != nil {
			return st, errors.Join(append(errs, err)...)
		}
		resolved := false
		for _, op := range due {
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

// reconcile searches the target's comments for the op's exact rendered body.
// One exact match is positive evidence and applies the op. Anything else leaves
// it remote_unknown: no match is not proof the write did not happen, so there
// is no automatic re-create.
func (p *Protocol) reconcile(ctx context.Context, op Op) (applied bool, err error) {
	touch := func(reason, detail string) (bool, error) {
		now := p.now()
		_, terr := p.Ledger.TouchUnknown(ctx, now, op.ID, now.Add(p.reconcileDelay(op.Reconciles+1)), reason, detail)
		return false, terr
	}
	want, rerr := RenderComment(op)
	if rerr != nil {
		return touch(ReasonReconcileFailed, rerr.Error())
	}
	cctx, cancel := p.detached(ctx)
	defer cancel()
	marker := OpMarker(op.ID)
	var matches []RemoteComment
	deleted, mismatched := 0, 0
	comments, lerr := p.Remote.ListComments(cctx, op.YTTargetID, p.PageSize)
	if lerr != nil {
		return touch(ReasonReconcileFailed, lerr.Error())
	}
	for _, c := range comments {
		if !strings.Contains(c.Text, marker) {
			continue
		}
		switch {
		case c.Text != want:
			mismatched++
		case c.Deleted:
			deleted++
		default:
			matches = append(matches, c)
		}
	}
	switch {
	case len(matches) == 1 && deleted == 0 && mismatched == 0 && resultIDRE.MatchString(matches[0].ID):
		s := p.apply(ctx, op, matches[0].ID)
		return s.err == nil, s.err
	case len(matches) > 1:
		return touch(ReasonMultipleMatches, fmt.Sprintf("%d identical comments carry the marker", len(matches)))
	case deleted > 0:
		return touch(ReasonMarkerDeleted, "the marked comment was deleted")
	case mismatched > 0:
		return touch(ReasonMarkerMismatch, "a comment carries the marker with other text")
	}
	return touch(ReasonNoEvidence, "no comment carries the marker")
}

// Resolution is an operator's decision on a remote_unknown op after looking at
// YouTrack themselves.
type Resolution struct {
	// Applied says the comment exists, and YTID names it. False says it does
	// not exist and the op is closed as rejected.
	Applied bool
	YTID    string
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
	var got Op
	if r.Applied {
		if !resultIDRE.MatchString(r.YTID) {
			return Result{}, fmt.Errorf("%w: youtrack comment id is empty or malformed", ErrInvalid)
		}
		got, err = p.Ledger.Apply(ctx, p.now(), id, r.YTID, []MapRow{commentMap(op, r.YTID)})
	} else {
		got, err = p.Ledger.Reject(ctx, p.now(), id, ReasonOperatorNotDone, "by "+r.By+": "+r.Note)
	}
	if err != nil {
		return Result{}, err
	}
	return p.result(got, false), nil
}

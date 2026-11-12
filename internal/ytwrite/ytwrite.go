// Package ytwrite is the write-operation protocol hk uses to write to YouTrack
// (hk 3667 v2, hk 3697). YouTrack has no transactions, no conditional update
// and no idempotency key, so hk supplies them with a durable op log (the
// Ledger): every logical write is one row that is committed before any remote
// call, carries its immutable outbound payload, and moves through
//
//	prepared -> dispatched -> applied | rejected | remote_unknown
//
// The package holds the rules only. It imports no database package: the
// Postgres Ledger lives in internal/store, the in-memory Ledger and the fake
// YouTrack that the fault tests drive live in internal/ytwrite/ytwritetest,
// and the same contract suite runs against both. The first production writer
// is slice b1a (hk 3739): the store prepares a comment_append op in an hk
// comment's own transaction for a task in the comment canary, and serve's
// worker sends it with Drive over the YouTrack adapter
// (internal/youtrack/write_remote.go).
//
// The rules, each pinned by a fault test:
//
//   - One remote attempt per dispatch. A create whose reply was lost is never
//     sent again: it stays remote_unknown until positive evidence (the exact
//     rendered body, marker included, found by enumerating the target's
//     comments) adopts it. Absence is never proof; it keeps remote_unknown.
//   - A caller is told "pending" or "applied" or "rejected", never a success
//     the ledger has not recorded. Pending is promised only after the op row
//     is durable.
//   - An idempotency key is scoped to (principal, kind). The same key with the
//     same payload returns the recorded result; with another payload it is
//     ErrKeyConflict.
//   - Writes to one entity are ordered: an op is dispatched only when every
//     earlier op on the same entity is resolved.
package ytwrite

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// State is one op's place in the lifecycle.
type State string

const (
	// StatePrepared: the row is durable and no remote call has been made. A
	// 429 or 401 puts a dispatched op back here with a later NextAttemptAt:
	// the request was not processed.
	StatePrepared State = "prepared"
	// StateDispatched: dispatch ownership is committed and the remote call is
	// (or was) in flight. A crash here is uncertain, not safe to resend.
	StateDispatched State = "dispatched"
	// StateApplied: the remote effect is confirmed and its YouTrack id and map
	// rows are committed. Terminal.
	StateApplied State = "applied"
	// StateRejected: a final refusal, or an operator's "it did not happen".
	// Terminal.
	StateRejected State = "rejected"
	// StateRemoteUnknown: the effect may or may not exist. Only positive
	// evidence or an operator decision leaves this state.
	StateRemoteUnknown State = "remote_unknown"
)

// Valid reports whether s is one of the five states.
func (s State) Valid() bool {
	switch s {
	case StatePrepared, StateDispatched, StateApplied, StateRejected, StateRemoteUnknown:
		return true
	}
	return false
}

// Resolved reports a terminal state.
func (s State) Resolved() bool { return s == StateApplied || s == StateRejected }

// Status is the three-way answer a caller sees.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApplied  Status = "applied"
	StatusRejected Status = "rejected"
)

// KindCommentAppend is the first registered kind (kind_comment.go): one
// comment appended to a mapped task's issue.
const KindCommentAppend = "comment_append"

// KindIssueCreate (kind_issue.go, hk 3898) creates a task's YouTrack issue,
// or adopts the one issue that already carries the task's hk ID.
const KindIssueCreate = "issue_create"

// Reasons a caller can read on a Result. They are stable codes.
const (
	ReasonDispatched        = "dispatched"
	ReasonRemoteUnknown     = "remote_unknown"
	ReasonRateLimited       = "rate_limited"
	ReasonUnauthorized      = "unauthorized"
	ReasonWaitingForEarlier = "waiting_for_earlier_op"
	ReasonOutcomeUnrecorded = "outcome_unrecorded"
	ReasonNoID              = "no_id_in_reply"
	ReasonTransport         = "transport_error"
	ReasonServerError       = "http_5xx"
	ReasonDispatcherLost    = "dispatcher_lost"
	ReasonNoEvidence        = "no_evidence"
	ReasonMultipleMatches   = "multiple_matches"
	ReasonMarkerMismatch    = "marker_mismatch"
	ReasonMarkerDeleted     = "marker_deleted"
	ReasonReconcileFailed   = "reconcile_failed"
	ReasonOperatorNotDone   = "operator_not_applied"
	ReasonUnsupportedKind   = "unsupported_kind"
	// ReasonNotSent: a read a kind makes before its write failed, so nothing
	// was sent; the op is prepared again (NotSentError).
	ReasonNotSent = "not_sent"
	// Local refusals (slice b1a): the comment row commits and its op is
	// rejected in the same transaction, before anything is sent.
	ReasonInvalidComment = "invalid_comment"
	ReasonAmbiguousIssue = "ambiguous_issue"
	// ReasonInvalidIssue: a task whose issue the protocol cannot render (an
	// actor the provenance line cannot carry, a title quoting the marker).
	ReasonInvalidIssue = "invalid_issue"
	// ReasonBindingConflict is set on an applied issue_create op whose
	// binding was not published because the task or the issue was already
	// bound elsewhere (hk 3898). The op stays applied; the operator's view
	// lists it with the rejected ops.
	ReasonBindingConflict = "binding_conflict"
	// ReasonProjectChanged: an issue_create op not yet sent whose task moved
	// to another project; the store rejects it and prepares the task's
	// current project's op in the same transaction (hk 3909).
	ReasonProjectChanged = "project_changed"
	// ReasonHeld: the ledger's claim refused the dispatch (ClaimHeld:
	// canary removed, mode off, another tenant); the op stays prepared and
	// nothing is sent.
	ReasonHeld = "held"
)

var (
	// ErrInvalid is a request the protocol refuses before anything is stored.
	ErrInvalid = errors.New("ytwrite_invalid")
	// ErrKeyConflict is the same (principal, kind, key) with another payload.
	ErrKeyConflict = errors.New("ytwrite_key_conflict")
	// ErrNotFound is an op id the principal cannot see.
	ErrNotFound = errors.New("ytwrite_not_found")
	// ErrState is a transition the lifecycle does not allow.
	ErrState = errors.New("ytwrite_state")
	// ErrConflictingResult is a second, different YouTrack id for an op that
	// already has one: a duplicate effect. It is reported, never overwritten.
	ErrConflictingResult = errors.New("ytwrite_conflicting_result")
)

// Op is one logical write as the ledger holds it. Principal, Kind, Key,
// PayloadSHA, Payload, EntityKind, EntityID, YTTargetID, Actor, ParentID,
// ChildSeq and CreatedAt never change after Prepare.
type Op struct {
	ID        int64
	Principal string
	Kind      string
	Key       string
	// PayloadSHA is the hash that tells a replay from a conflicting reuse of
	// the key. It covers what the write means (kind, target, actor, body), not the
	// time.
	PayloadSHA string
	// Payload is the immutable outbound payload (JSON); the exact text sent is
	// a pure function of it and the op, so a resend or a reconciliation
	// renders the same bytes.
	Payload []byte
	// EntityKind and EntityID name the hk entity whose writes are ordered
	// (task, 3697). YTTargetID is the YouTrack id of the thing written to.
	EntityKind string
	EntityID   string
	YTTargetID string
	// Tenant is the YouTrack host the op was bound to when it was prepared.
	// It is part of the write's identity (PayloadSHA): only a dispatcher
	// configured for that tenant sends or searches for it.
	Tenant string
	Actor  string
	// ParentID and ChildSeq record a parent/child plan (0 = none). The schema
	// carries them for multi-step writes; this slice does not settle parents.
	ParentID int64
	ChildSeq int

	State State
	// YTResultID is the YouTrack id of the created thing, set on applied.
	YTResultID string
	// Reason is the latest cause code (terminal or pending); LastError is the
	// free-form detail behind it.
	Reason    string
	LastError string
	// Attempts counts dispatches; Reconciles counts evidence searches.
	Attempts   int
	Reconciles int

	CreatedAt     time.Time
	DispatchedAt  time.Time
	NextAttemptAt time.Time
	ResolvedAt    time.Time
}

// NewOp is what Prepare stores.
type NewOp struct {
	Principal  string
	Kind       string
	Key        string
	PayloadSHA string
	Payload    []byte
	EntityKind string
	EntityID   string
	YTTargetID string
	Tenant     string
	Actor      string
	ParentID   int64
	ChildSeq   int
}

// MapRow is one hk identity -> YouTrack id row committed with an op's
// `applied`.
type MapRow struct {
	EntityKind string
	HKKey      string
	YTID       string
	YTParentID string
	OpID       int64
	// Adopted: the YouTrack thing existed before the op, which adopted it
	// (AdoptedError) instead of creating it. Not stored in the map; the
	// ledger may use it for what it publishes with the op.
	Adopted bool
}

// ClaimOutcome says why Claim did or did not start a dispatch.
type ClaimOutcome string

const (
	ClaimClaimed     ClaimOutcome = "claimed"
	ClaimNotPrepared ClaimOutcome = "not_prepared"
	ClaimNotDue      ClaimOutcome = "not_due"
	ClaimBlocked     ClaimOutcome = "blocked"
	// ClaimHeld: the ledger does not admit the dispatch now (for the comment
	// canary: the task left the canary, the mode is off, or the op is bound
	// to another tenant). Nothing is sent and the op stays prepared.
	ClaimHeld ClaimOutcome = "held"
)

// Ledger is the durable op log. Every method is one atomic step; a method that
// moves state is a compare-and-set on the current state, so concurrent callers
// and a second process cannot both win it. Time is always passed in (one clock
// for the whole table); the ledger never reads its own.
type Ledger interface {
	// Prepare stores n as a new prepared op, or returns the op already stored
	// under (principal, kind, key). created is false for the replay. The same
	// key with another PayloadSHA is ErrKeyConflict.
	Prepare(ctx context.Context, now time.Time, n NewOp) (op Op, created bool, err error)
	// Get returns an op the principal owns; any other id is ErrNotFound.
	Get(ctx context.Context, principal string, id int64) (Op, error)
	// ByID returns an op by id for the protocol's own recovery paths.
	ByID(ctx context.Context, id int64) (Op, error)
	// Claim starts a dispatch: prepared -> dispatched, attempts+1, only when
	// the op is due, the ledger admits it (else ClaimHeld) and every earlier op
	// on its entity is resolved. Admission is decided in the claim's own
	// atomic step, so a change that revokes it either precedes the claim and
	// holds the op or follows a committed claim.
	Claim(ctx context.Context, now time.Time, id int64) (Op, ClaimOutcome, error)
	// Apply records the confirmed effect and commits maps in the same
	// transaction. From dispatched or remote_unknown. Applying an applied op
	// with the same id is a no-op; with another id it is ErrConflictingResult.
	Apply(ctx context.Context, now time.Time, id int64, ytID string, maps []MapRow) (Op, error)
	// Reject records a final refusal. From prepared, dispatched or
	// remote_unknown.
	Reject(ctx context.Context, now time.Time, id int64, reason, detail string) (Op, error)
	// Defer puts a dispatched op back to prepared, due at next: the remote did
	// not process the request.
	Defer(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (Op, error)
	// MarkUnknown moves a dispatched op to remote_unknown, due for evidence
	// search at next. Idempotent for an op already remote_unknown.
	MarkUnknown(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (Op, error)
	// RecoverStale marks only the scanned dispatch attempt when it is still stale.
	RecoverStale(ctx context.Context, id int64, attempts int, cutoff, next time.Time, detail string) (bool, error)
	// TouchUnknown records a search that found no evidence: reconciles+1, due
	// again at next. Only for remote_unknown.
	TouchUnknown(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (Op, error)
	// Due lists prepared and remote_unknown ops whose NextAttemptAt <= now,
	// ascending id. A prepared op queued behind an unresolved earlier op on its
	// entity, or one Claim would hold, is not listed (it cannot be claimed now,
	// and listing it would let a long blocked or held queue crowd out ops that
	// can run). A remote_unknown op is listed unless it is bound to a tenant the
	// ledger does not serve; the evidence search is read-only.
	Due(ctx context.Context, now time.Time, limit int) ([]Op, error)
	// Stale lists dispatched ops whose dispatch began at or before cutoff,
	// ascending id.
	Stale(ctx context.Context, cutoff time.Time, limit int) ([]Op, error)
	// ListState lists ops in one state, ascending id: the operator's view of
	// what is unresolved.
	ListState(ctx context.Context, state State, limit int) ([]Op, error)
	// MapGet reads one committed map row.
	MapGet(ctx context.Context, entityKind, hkKey string) (MapRow, bool, error)
}

// RemoteComment is one comment as the remote lists it.
type RemoteComment struct {
	ID        string
	Text      string
	UpdatedAt time.Time
	Deleted   bool
	// AuthorID is the YouTrack user id of the comment's author ("" when
	// the remote did not say). The comment audit tells the bot's comments
	// from people's by it; the protocol never reads it.
	AuthorID string
}

// Remote is the YouTrack surface the protocol uses. CreateComment makes exactly
// one attempt: no retry inside, because a retry after a lost reply could
// duplicate the comment.
type Remote interface {
	// CreateComment appends text to the issue. A nil error with an empty id is
	// a 2xx without a usable id; the protocol treats it as uncertain.
	CreateComment(ctx context.Context, issueID, text string) (id string, err error)
	// ListComments returns a complete, consistent set of comments, or an error.
	// Offset-paging adapters must use EnumerateStableComments and propagate its
	// error (ErrInconsistentEnumeration, ErrEnumerationTooLarge). Each comment
	// carries its revision time and deletion flag, so two reads compare the
	// actual state. A 429 is a *RemoteError with the server's RetryAfter; the
	// next search waits at least that long.
	ListComments(ctx context.Context, issueID string, pageSize int) ([]RemoteComment, error)
}

// RemoteIssue is one issue as an issue search returns it.
type RemoteIssue struct {
	ID          string
	Readable    string
	ProjectID   string
	Summary     string
	Description string
	// Created is when YouTrack created the issue (zero: not reported).
	Created time.Time
}

// IssueCreate is one issue create: the project, the text, and the task whose
// hk ID and hk project the issue carries (the only fields a create writes).
type IssueCreate struct {
	ProjectID   string
	Summary     string
	Description string
	TaskID      int64
	HKProject   string
	// HKProjectText sends the hk project as text instead of an enum value.
	HKProjectText bool
}

// IssueFields is how the project's fields take an issue_create write.
type IssueFields struct {
	// ProjectText: the hk project field is a text field (the importer's
	// shape), not an enum (the legacy push's).
	ProjectText bool
}

// IssueRemote is the YouTrack surface of the issue_create kind. A Remote
// that does not implement it cannot send that kind (the op is rejected
// unsupported_kind before anything is sent).
type IssueRemote interface {
	// CheckIssueFields reports whether the project can take an issue_create
	// write: it has the hk ID field and the hk project field with hkProject
	// among its values (when hkProject is not ""). A missing field or value
	// is a plain error (the op is held, not rejected, until the schema is
	// repaired); a failed read follows CreateComment's errors.
	CheckIssueFields(ctx context.Context, projectID, hkProject string) (IssueFields, error)
	// FindTaskIssues returns every issue of the project (short name and id)
	// whose hk ID is taskID, checked against both, or an error. A result it
	// cannot prove complete is an error. A 429 is a *RemoteError with the
	// server's RetryAfter.
	FindTaskIssues(ctx context.Context, projectShort, projectID string, taskID int64) ([]RemoteIssue, error)
	// CreateIssue makes exactly one POST and returns the new issue's id. Its
	// errors follow CreateComment's.
	CreateIssue(ctx context.Context, in IssueCreate) (id string, err error)
}

// RemoteError is an HTTP answer from the remote. Any other error from Remote
// is a transport failure and is uncertain.
type RemoteError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("youtrack answered %d: %s", e.Status, e.Message)
}

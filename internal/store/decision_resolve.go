package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// ResolveDecision is the one entry that closes a structured decision request
// (hk 3061; design hk:doc design/2026-11-05/hk-work-record-design-v1.1 §0-3,
// §10.2, §10.3, §13.1 R1/R2). The CLI, the assistant relay and the console
// all come through it, and in one transaction it:
//
//   - writes one append-only decision_answer_attempts row, whatever the
//     result;
//   - writes at most one decision_resolutions row per logical request and
//     question revision;
//   - keeps the source record (tasks.refs.decision_request), the task event,
//     the notification outbox, the mirror and the Discord close post;
//   - returns exactly one result of the R2 enum.
//
// Provenance is derived from the verified channel, never read from the body:
// operator_direct needs Verified (the console's Access email identity, or the
// bearer client "operator"); anything else an operator said arrives as
// operator_relay, and a human_only request does not close on a relay — the
// attempt waits as pending_confirmation until a verified path answers.

// Answer channels. They are the values of decision_answer_attempts.channel
// that ResolveDecision accepts; linear, lane and default are reserved by the
// table for the routes that are not yet moved here.
const (
	DecisionChannelCLI       = "cli"
	DecisionChannelAssistant = "assistant"
	DecisionChannelConsole   = "console"
)

// ResolveDecision results (R2). A result is the classification of one answer
// attempt; every one of them commits its attempt row.
const (
	DecisionResultAnswered            = "answered"
	DecisionResultConflict            = "conflict"
	DecisionResultStaleRevision       = "stale_revision"
	DecisionResultPendingConfirmation = "pending_confirmation"
	DecisionResultExpired             = "expired"
	DecisionResultSuperseded          = "superseded"
)

// decisionAttemptRefused is the attempt-only result of an assistant judgement
// on a human_only request: ResolveDecision returns the error, not a result.
const decisionAttemptRefused = "refused"

// Resolution provenance (decision_resolutions.provenance). Stored refs keep
// their own vocabulary (operator_decided / assistant_judgement).
const (
	DecisionProvenanceOperatorDirect = "operator_direct"
	DecisionProvenanceOperatorRelay  = "operator_relay"
)

// DecisionEventKeyMaxBytes bounds an event key.
const DecisionEventKeyMaxBytes = 128

var eventKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

func validEventKey(key string) bool {
	return len(key) <= DecisionEventKeyMaxBytes && eventKeyRE.MatchString(key)
}

// ResolveDecisionInput is one attempt to close a request. Channel, Actor and
// Verified are set by the calling handler from the authenticated request,
// never from a request body.
type ResolveDecisionInput struct {
	// TaskID is optional: the task whose request is addressed. Absent, it is
	// the task named by RequestID. A RequestID that belongs to another task is
	// superseded for this one, as a stale id always was.
	TaskID int64
	// RequestID is the dr- id the caller saw; it fixes the question revision.
	RequestID string
	Kind      string
	Option    string
	Text      string
	Receipt   string
	Responder string
	// EventKey identifies the delivery: the same key with the same payload
	// replays the first result and writes nothing, with another payload it is
	// ErrDecisionEventKeyConflict. Absent, it is "auto:" + the payload hash.
	EventKey string

	Channel  string
	Actor    string
	Verified bool

	// The assistant claim (channel assistant only): the provenance it names,
	// the operator's verbatim words and the request id they are bound to.
	Provenance    string
	OperatorQuote string
	DecisionRef   string
}

// ResolveDecisionOutcome is the result of one attempt. Result is one of the
// DecisionResult* values; for a conflict, stale or superseded result, Refusal
// is the sentinel the legacy callers answer with (ErrDecisionRequestStale or
// ErrDecisionRequestResolved). Duplicate marks an identical re-send, or a
// replay of an earlier delivery.
type ResolveDecisionOutcome struct {
	Result       string
	Refusal      error
	Duplicate    bool
	ResolutionID int64
	AttemptID    int64
	Task         Task
	Request      DecisionRequest
}

// legacyResult maps an outcome back to the shape and sentinels the two
// adapters have always returned.
func (o ResolveDecisionOutcome) legacyResult(err error) (DecisionRequestResult, error) {
	switch {
	case err != nil:
		return DecisionRequestResult{}, err
	case o.Refusal != nil:
		return DecisionRequestResult{}, o.Refusal
	case o.Result == DecisionResultPendingConfirmation:
		return DecisionRequestResult{}, ErrDecisionPendingConfirmation
	}
	return DecisionRequestResult{Task: o.Task, Request: o.Request, Duplicate: o.Duplicate, Result: o.Result, ResolutionID: o.ResolutionID, AttemptID: o.AttemptID}, nil
}

// decisionPayload is what the payload hash covers: everything that makes two
// deliveries the same answer.
type decisionPayload struct {
	TaskID            int64  `json:"task_id"`
	RevisionRequestID string `json:"revision_request_id"`
	Kind              string `json:"kind"`
	Option            string `json:"option"`
	Text              string `json:"text"`
	Receipt           string `json:"receipt"`
	Responder         string `json:"responder"`
	Provenance        string `json:"provenance"`
	Quote             string `json:"quote"`
	DecisionRef       string `json:"decision_ref"`
	Actor             string `json:"actor"`
}

func (in ResolveDecisionInput) payloadHash(taskID int64) (string, error) {
	raw, err := json.Marshal(decisionPayload{
		TaskID: taskID, RevisionRequestID: in.RequestID, Kind: in.Kind, Option: in.Option, Text: in.Text,
		Receipt: in.Receipt, Responder: in.Responder, Provenance: in.Provenance, Quote: in.OperatorQuote,
		DecisionRef: in.DecisionRef, Actor: in.Actor,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// decisionAttempt is one decision_answer_attempts row to insert.
type decisionAttempt struct {
	channel, eventKey, hash string
	taskID                  int64
	root                    string
	revisionRequestID       string
	revision                int
	provenance              string
	actor, result           string
	at                      time.Time
}

func insertDecisionAttempt(ctx context.Context, tx pgx.Tx, a decisionAttempt) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,request_id,revision_request_id,question_revision,provenance,actor,result,received_at)
		VALUES($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,0),NULLIF($8,''),$9,$10,$11) RETURNING id`,
		a.channel, a.eventKey, a.hash, a.taskID, a.root, a.revisionRequestID, a.revision, a.provenance, a.actor, a.result, a.at).Scan(&id)
	return id, err
}

// derivedProvenance is the provenance of an answer, from the verified channel
// and never from the body: a Verified channel is operator_direct; an assistant
// answer is operator_relay with a visible operator quote and
// assistant_judgement without; any other unverified answer is operator_relay.
// It is empty when the assistant's claim is not a known value.
func derivedProvenance(in ResolveDecisionInput, prov *assistantProvenance) string {
	switch {
	case in.Verified:
		return DecisionProvenanceOperatorDirect
	case prov == nil:
		return DecisionProvenanceOperatorRelay
	case !prov.valid():
		return ""
	case prov.effective() == DecisionProvenanceOperatorDecided:
		return DecisionProvenanceOperatorRelay
	}
	return DecisionProvenanceAssistantJudgement
}

// decidedBy names who decided, for decision_resolutions.decided_by.
func decidedBy(in ResolveDecisionInput, prov *assistantProvenance) string {
	switch {
	case prov != nil:
		return prov.decidedBy()
	case in.Verified:
		return DecisionDecidedByOperator
	case in.Responder != "":
		return in.Responder
	}
	return in.Actor
}

// ResolveDecision classifies one answer to a dr- request and records it; see
// the file comment. Validation failures, and a wrk fence refusal, return an
// error and write nothing. A human_only request answered by an assistant
// judgement is refused: the attempt is recorded and the error returned.
func (s *Store) ResolveDecision(ctx context.Context, in ResolveDecisionInput) (ResolveDecisionOutcome, error) {
	assistant := in.Channel == DecisionChannelAssistant
	switch in.Channel {
	case DecisionChannelCLI, DecisionChannelAssistant, DecisionChannelConsole:
	default:
		return ResolveDecisionOutcome{}, invalidDecisionRequest("channel must be cli, assistant or console")
	}
	if in.Verified && assistant {
		return ResolveDecisionOutcome{}, invalidDecisionRequest("the assistant channel is never verified")
	}
	if in.Actor == "" || !validText(in.Actor, 128) {
		return ResolveDecisionOutcome{}, invalidDecisionRequest("recorder is required")
	}
	if in.EventKey != "" && !validEventKey(in.EventKey) {
		return ResolveDecisionOutcome{}, invalidDecisionRequest(fmt.Sprintf("event_key must be at most %d bytes of letters, digits, . _ : -", DecisionEventKeyMaxBytes))
	}
	effective := DecisionResolveInput{RequestID: in.RequestID, Kind: in.Kind, Option: in.Option, Text: in.Text, Receipt: in.Receipt, Responder: in.Responder}
	var prov *assistantProvenance
	if assistant {
		if in.Kind != "" && in.Kind != DecisionRequestAnswered {
			return ResolveDecisionOutcome{}, ErrDecisionAssistantKind
		}
		p := normalizeAssistantProvenance(in)
		prov = &p
		effective.Kind = DecisionRequestAnswered
		effective.Responder = p.responder()
	} else if in.Provenance != "" || in.OperatorQuote != "" || in.DecisionRef != "" {
		// Only the assistant relays a claim; a body that names operator_direct
		// (or any provenance) on another channel is refused, not trusted.
		return ResolveDecisionOutcome{}, ErrInvalidProvenance
	}
	if err := CheckDecisionResolveInput(effective); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err := guard.Reject(strings.Join([]string{in.Text, in.Receipt, effective.Responder}, "\n")); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	parsedTask, revision, parsed := ParseDecisionRequestID(in.RequestID)
	taskID := in.TaskID
	if taskID == 0 {
		if !parsed {
			return ResolveDecisionOutcome{}, invalidDecisionRequest("request_id is required (dr-<task>-<revision>)")
		}
		taskID = parsedTask
	}
	if taskID < 1 {
		return ResolveDecisionOutcome{}, invalidDecisionRequest("task id must be positive")
	}
	hash, err := in.payloadHash(taskID)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	eventKey := in.EventKey
	if eventKey == "" {
		eventKey = "auto:" + hash
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResolveDecisionOutcome{}, ErrTaskNotFound
		}
		return ResolveDecisionOutcome{}, err
	}
	if err = s.wrkTaskFenceTx(ctx, tx, x, "claimed", false); err != nil {
		return ResolveDecisionOutcome{}, err
	}

	// A replay answers what the first delivery got, even after a newer
	// revision exists, so the key is looked up before any classification.
	replay, found, err := replayDecisionAttempt(ctx, tx, in.Channel, eventKey, hash, x)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if found {
		return replay.out, replay.err
	}

	decisionBefore := decisionBeforeOf(x)
	current := x.Refs.DecisionRequest
	provenance := derivedProvenance(in, prov)
	now := time.Now().UTC()
	attempt := decisionAttempt{
		channel: in.Channel, eventKey: eventKey, hash: hash, taskID: x.ID, revisionRequestID: in.RequestID,
		provenance: provenance, actor: in.Actor, at: now,
	}
	if parsed {
		attempt.revision = revision
	}
	var currentCopy DecisionRequest
	if current != nil {
		currentCopy = *current
	}
	// record commits the attempt alone, the outcome of every result that
	// changes no request.
	record := func(result string, refusal error) (ResolveDecisionOutcome, error) {
		attempt.result = result
		id, err := insertDecisionAttempt(ctx, tx, attempt)
		if err != nil {
			return ResolveDecisionOutcome{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return ResolveDecisionOutcome{}, err
		}
		return ResolveDecisionOutcome{Result: result, Refusal: refusal, AttemptID: id, Task: x, Request: currentCopy}, nil
	}

	inChain := current != nil && parsed && parsedTask == x.ID
	if !inChain {
		return record(DecisionResultSuperseded, ErrDecisionRequestStale)
	}
	root := current.RootID()
	attempt.root = root
	if in.RequestID != current.ID {
		// A revision of the current logical request that a later revision
		// replaced is stale_revision; any other id is not in the chain.
		if _, rootRevision, ok := ParseDecisionRequestID(root); ok && revision >= rootRevision && revision < current.Revision {
			return record(DecisionResultStaleRevision, ErrDecisionRequestStale)
		}
		return record(DecisionResultSuperseded, ErrDecisionRequestStale)
	}

	if current.Status != DecisionRequestOpen {
		if assistant {
			// Amendment 1 (A2): provenance validity and the operator_decided
			// ref binding are judged before the duplicate comparison — an
			// invalid claim or a quote bound to another request id on an
			// already-resolved request is a 400/409, never a duplicate.
			if !prov.valid() {
				return ResolveDecisionOutcome{}, ErrInvalidProvenance
			}
			if prov.effective() == DecisionProvenanceOperatorDecided && prov.ref != in.RequestID {
				return ResolveDecisionOutcome{}, ErrProvenanceRefMismatch
			}
		}
		if current.Resolution != nil && sameResolution(*current.Resolution, effective, prov) {
			attempt.result = DecisionResultAnswered
			id, err := insertDecisionAttempt(ctx, tx, attempt)
			if err != nil {
				return ResolveDecisionOutcome{}, err
			}
			resolutionID, err := decisionResolutionID(ctx, tx, root, current.Revision)
			if err != nil {
				return ResolveDecisionOutcome{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return ResolveDecisionOutcome{}, err
			}
			return ResolveDecisionOutcome{Result: DecisionResultAnswered, Duplicate: true, ResolutionID: resolutionID, AttemptID: id, Task: x, Request: currentCopy}, nil
		}
		switch current.Status {
		case DecisionRequestDefaultApplied:
			return record(DecisionResultExpired, ErrDecisionRequestResolved)
		case DecisionRequestWithdrawn:
			return record(DecisionResultSuperseded, ErrDecisionRequestResolved)
		}
		return record(DecisionResultConflict, ErrDecisionRequestResolved)
	}

	if assistant {
		// An open request left on a merged or dropped task is uncleaned
		// state, not answerable work — the pending list already excludes it.
		// Only the operator path closes it, as request cleanup.
		if isTerminalTaskState(x.State) {
			return ResolveDecisionOutcome{}, ErrTaskTerminal
		}
		// A disposition item is answered only on the operator's
		// Access-authenticated web route. Provenance never widens that: no
		// quote closes it here.
		if x.Refs.Disposition != nil {
			return ResolveDecisionOutcome{}, ErrDispositionOperatorOnly
		}
		// Provenance validation: the claim must name a known value, a
		// supplied quote obeys the answer-text rules, and an operator_decided
		// answer's ref must bind this exact request id — a quote for one
		// decision or revision never answers another.
		if !prov.valid() {
			return ResolveDecisionOutcome{}, ErrInvalidProvenance
		}
		if quoteVisible(prov.quote) {
			if !validText(prov.quote, DecisionOperatorQuoteMaxBytes) || !utf8.ValidString(prov.quote) {
				return ResolveDecisionOutcome{}, invalidDecisionRequest(fmt.Sprintf("operator_quote must be at most %d bytes of valid text", DecisionOperatorQuoteMaxBytes))
			}
			if err := guard.Reject(prov.quote); err != nil {
				return ResolveDecisionOutcome{}, err
			}
		}
		if prov.effective() == DecisionProvenanceOperatorDecided && prov.ref != in.RequestID {
			return ResolveDecisionOutcome{}, ErrProvenanceRefMismatch
		}
		// A human_only request names the boundary in the request itself: a
		// judgement never closes it, and neither does a claim without the
		// operator's quote. The refusal is recorded as an attempt.
		if current.HumanOnly && prov.effective() != DecisionProvenanceOperatorDecided {
			var refused error = ErrDecisionRequestHumanOnly
			if prov.claimed == DecisionProvenanceOperatorDecided {
				refused = fmt.Errorf("%w: no_operator_quote", ErrDecisionRequestHumanOnly)
			}
			out, err := record(decisionAttemptRefused, nil)
			if err != nil {
				return ResolveDecisionOutcome{}, err
			}
			out.Result = ""
			return out, refused
		}
	}
	if in.Option != "" {
		if x.Refs.DecisionOptions == nil || !optionKeys(*x.Refs.DecisionOptions)[in.Option] {
			return ResolveDecisionOutcome{}, invalidDecisionRequest("option " + in.Option + " is not one of the request's options")
		}
	}
	if effective.Kind == DecisionRequestAnswered && in.Option == "" && (x.Refs.DecisionOptions == nil || !x.Refs.DecisionOptions.AllowFree) {
		return ResolveDecisionOutcome{}, invalidDecisionRequest("this request does not allow a free answer; name an option")
	}
	// R1: a human_only request closes only on a verified path. An answer
	// relayed by an assistant or a session waits for the operator.
	if effective.Kind == DecisionRequestAnswered && current.HumanOnly && provenance != DecisionProvenanceOperatorDirect {
		return record(DecisionResultPendingConfirmation, nil)
	}

	attempt.result = DecisionResultAnswered
	attemptID, err := insertDecisionAttempt(ctx, tx, attempt)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	resolved := *current
	resolved.Status = effective.Kind
	resolved.Resolution = &DecisionResolution{Kind: effective.Kind, Option: in.Option, Text: in.Text, Receipt: in.Receipt, Responder: effective.Responder, By: in.Actor, At: now}
	if assistant {
		resolved.Resolution.Provenance = prov.effective()
		resolved.Resolution.OperatorQuote = prov.recordedQuote()
		resolved.Resolution.DecisionRef = prov.recordedRef()
		resolved.Resolution.DecidedBy = prov.decidedBy()
	}
	x.Refs.DecisionRequest = &resolved
	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET refs=$2::jsonb,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, x.ID, string(encoded), now), &x); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	var resolutionID int64
	err = tx.QueryRow(ctx, `INSERT INTO decision_resolutions(request_id,question_revision,revision_request_id,task_id,kind,option_id,text,receipt,provenance,channel,decided_by,recorded_by,attempt_id,committed_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,$12,$13,$14) RETURNING resolution_id`,
		root, current.Revision, resolved.ID, x.ID, effective.Kind, in.Option, in.Text, in.Receipt, provenance, in.Channel,
		decidedBy(in, prov), in.Actor, attemptID, now).Scan(&resolutionID)
	if err != nil {
		return ResolveDecisionOutcome{}, err
	}
	note := "decision-" + effective.Kind + " " + resolved.ID
	if in.Option != "" {
		note += " " + in.Option
	}
	if in.Receipt != "" {
		note += " receipt " + in.Receipt
	}
	if assistant {
		note += " " + prov.marker()
	}
	if in.Text != "" {
		note += ": " + in.Text
	}
	if assistant && prov.effective() == DecisionProvenanceOperatorDecided {
		note += " — " + prov.quote
	}
	if assistant {
		// The notification is written before the decision event so a failure
		// anywhere later in the transaction still rolls both back together.
		head := "[via berry] " + resolved.ID + " answered"
		if in.Option != "" {
			head += " " + in.Option
		}
		head += " " + prov.marker()
		if in.Text != "" {
			head += ": "
		}
		text := assistantLaneText(head, in.Text)
		if err = insertNotificationOutbox(ctx, tx, OutboxKindDecisionAnswered, x.Lane, decisionAnsweredEventID(resolved.ID), text, now); err != nil {
			return ResolveDecisionOutcome{}, err
		}
	}
	if err = insertDecisionEvent(ctx, tx, x.ID, x.State, in.Actor, note, x.Refs, now); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, x, in.Actor, note, 0, now); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err = queueDecisionClosedPost(ctx, tx, resolved, x.Refs.DecisionOptions, now); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err = s.recordLinearBoardTaskTx(ctx, tx, x, note); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ResolveDecisionOutcome{}, err
	}
	return ResolveDecisionOutcome{Result: DecisionResultAnswered, ResolutionID: resolutionID, AttemptID: attemptID, Task: x, Request: resolved}, nil
}

// decisionResolutionID is the id of the resolution row of one logical request
// and revision, 0 when none exists (a request answered before version 43).
func decisionResolutionID(ctx context.Context, q rowQuerier, root string, revision int) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT resolution_id FROM decision_resolutions WHERE request_id=$1 AND question_revision=$2`, root, revision).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// decisionReplay is a stored attempt returned for a re-sent delivery: the
// outcome and, for a refused one, the error the first delivery got.
type decisionReplay struct {
	out ResolveDecisionOutcome
	err error
}

// replayDecisionAttempt looks up (channel, event_key). With the same payload
// hash it rebuilds the first delivery's outcome and writes nothing; with
// another it is ErrDecisionEventKeyConflict. The rebuilt outcome carries the
// first delivery's result, sentinel, attempt id, resolution id and request;
// only Duplicate is set, and Task is the live task. A later confirmation of a
// pending_confirmation attempt is a new attempt under its own key: it never
// rewrites the pending one, so the pending replay still reports the open
// request the first delivery saw (R2).
func replayDecisionAttempt(ctx context.Context, tx pgx.Tx, channel, eventKey, hash string, x Task) (decisionReplay, bool, error) {
	var attemptID int64
	var revision int
	var storedHash, result, revisionRequestID, root string
	err := tx.QueryRow(ctx, `SELECT id,payload_hash,result,revision_request_id,COALESCE(request_id,''),COALESCE(question_revision,0) FROM decision_answer_attempts WHERE channel=$1 AND event_key=$2`, channel, eventKey).
		Scan(&attemptID, &storedHash, &result, &revisionRequestID, &root, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionReplay{}, false, nil
	}
	if err != nil {
		return decisionReplay{}, false, err
	}
	if storedHash != hash {
		return decisionReplay{}, false, ErrDecisionEventKeyConflict
	}
	out := ResolveDecisionOutcome{Result: result, Duplicate: true, AttemptID: attemptID, Task: x}
	if x.Refs.DecisionRequest != nil {
		out.Request = *x.Refs.DecisionRequest
	}
	// A pending attempt saw the request open; a later confirmation closed it.
	// Any other attempt reports the request it addressed, as it stands.
	wantStatus := ""
	if result == DecisionResultPendingConfirmation {
		wantStatus = DecisionRequestOpen
	}
	if out.Request.ID != revisionRequestID || (wantStatus != "" && out.Request.Status != wantStatus) {
		// The revision moved on or was closed; report the request as it was.
		var snapshot []byte
		err = tx.QueryRow(ctx, `SELECT refs->'decision_request' FROM task_events WHERE task_id=$1 AND kind='decision' AND refs->'decision_request'->>'id'=$2 AND ($3::text='' OR refs->'decision_request'->>'status'=$3::text) ORDER BY id DESC LIMIT 1`, x.ID, revisionRequestID, wantStatus).Scan(&snapshot)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return decisionReplay{}, false, err
		}
		var request DecisionRequest
		if ok, err := scanJSONB(snapshot, &request); err != nil {
			return decisionReplay{}, false, err
		} else if ok {
			out.Request = request
		}
	}
	err = tx.QueryRow(ctx, `SELECT resolution_id FROM decision_resolutions WHERE attempt_id=$1`, attemptID).Scan(&out.ResolutionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decisionReplay{}, false, err
	}
	if out.ResolutionID == 0 && result == DecisionResultAnswered && root != "" && revision > 0 {
		// An identical answer to a closed request is its own attempt without
		// a resolution row; it reports the one resolution of that revision.
		if out.ResolutionID, err = decisionResolutionID(ctx, tx, root, revision); err != nil {
			return decisionReplay{}, false, err
		}
	}
	switch result {
	case decisionAttemptRefused:
		out.Result = ""
		out.Duplicate = false
		return decisionReplay{out: out, err: ErrDecisionRequestHumanOnly}, true, nil
	case DecisionResultStaleRevision:
		out.Refusal = ErrDecisionRequestStale
	case DecisionResultConflict, DecisionResultExpired:
		out.Refusal = ErrDecisionRequestResolved
	case DecisionResultSuperseded:
		// A superseded result answers 409 stale for an id outside the chain,
		// and 409 resolved for a request that was withdrawn.
		out.Refusal = ErrDecisionRequestStale
		if out.Request.ID == revisionRequestID && out.Request.Status == DecisionRequestWithdrawn {
			out.Refusal = ErrDecisionRequestResolved
		}
	}
	return decisionReplay{out: out}, true, nil
}

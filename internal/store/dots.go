package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// dots is an external research AI that answers hk dots requests with replies
// "[dots] <KIND>" (ReplyDots). Each recorded reply becomes a document, a
// dots_results row and a lane notification. The record format (key, body,
// event id) is the one the retired Linear webhook path wrote, so documents
// and rows from before hk 4425 read the same.

const (
	// DotsLabel is the Linear label that puts an issue in dots' scope.
	DotsLabel = "dots"
	// DotsLane is the session of every dots document.
	DotsLane = "operator-desk"
)

// dotsResultKinds maps a comment kind that is recorded to the outbox kind that
// notifies about it. Everything else ("START", unknown words) is not recorded.
var dotsResultKinds = map[string]string{
	"RESULT":   OutboxKindDotsReport,
	"BLOCKED":  OutboxKindDotsBlocked,
	"QUESTION": OutboxKindDotsDecision,
}

// dotsNotifyLaneLineRE finds the "[알릴 레인] <lane>[, <lane>]" lines of a
// request body: the lanes that also hear a RESULT (operator decision 166).
var dotsNotifyLaneLineRE = regexp.MustCompile(`(?m)^\[알릴 레인\][ \t]*(.*?)[ \t]*\r?$`)

var (
	dotsIdentifierRE = regexp.MustCompile(`^[A-Za-z0-9_]+-[0-9]+$`)
)

// DotsIssue is the part of a Linear issue a dots record quotes. IssueUUID is
// Linear's durable id, the external_id of a task origin; Description is read
// for the hk-lane line only.
type DotsIssue struct {
	Identifier  string
	Title       string
	URL         string
	IssueUUID   string
	Description string
}

// DotsRequestInput describes a request filed directly in hk.
type DotsRequestInput struct {
	Title          string `json:"title"`
	Body           string `json:"body"`
	RequesterLane  string `json:"requester_lane,omitempty"`
	Project        string `json:"project,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Type           string `json:"type,omitempty"`
	// Options declares that the request ends in the operator's choice between
	// them (hk 4789): on a RESULT hk opens the operator's decision with these
	// options, their attachments and dots's recommendation. Empty is no
	// declaration and behaves as before. An option takes no recommended flag
	// here; dots names its recommendation on RESULT.
	Options []DecisionOption `json:"options,omitempty"`
	// DocKeys are documents the request cites (hk 6125). They are recorded as
	// task references in the same transaction as the request; the request's
	// binding is unchanged. Each key must name a stored document.
	DocKeys []string `json:"docs,omitempty"`
}

// ErrDotsRequestInvalid refuses a dots request the store cannot accept. Its
// text is the wire code (TestErrorRuleCodesMatchSentinels); the reason after
// "invalid_context: " is store-authored and is returned to the caller. A
// reason names the rule that failed and never echoes the caller's input.
var ErrDotsRequestInvalid = errors.New("invalid_context")

func dotsRequestInvalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrDotsRequestInvalid, reason)
}

// CreateDotsRequest creates a standalone request and its body in one transaction.
func (s *Store) CreateDotsRequest(ctx context.Context, by string, in DotsRequestInput) (Task, error) {
	lane := in.RequesterLane
	if lane == "" {
		lane = by
	}
	if by == "" || !validText(by, 128) {
		return Task{}, errors.New("invalid dots request")
	}
	if !validName(lane) {
		return Task{}, dotsRequestInvalid("requester lane must match [A-Za-z0-9._-]{1,128}")
	}
	if strings.TrimSpace(in.Body) == "" {
		return Task{}, dotsRequestInvalid("body must not be empty")
	}
	if err := checkDotsRequestTemplate(in); err != nil {
		// The template refuses with a plain, store-authored message. Wrapping
		// here gives every rule the one reason-bearing path, so a rule added to
		// the template carries its reason without touching this function.
		return Task{}, dotsRequestInvalid(err.Error())
	}
	if err := CheckDotsChoice(in.Options); err != nil {
		// The option rules are the decision request's, so their message carries
		// its sentinel; the dots route answers invalid_context like the rest of
		// this refusal path.
		reason, ok := strings.CutPrefix(err.Error(), ErrInvalidDecisionRequest.Error()+": ")
		if !ok {
			reason = err.Error()
		}
		return Task{}, dotsRequestInvalid(reason)
	}
	if err := rejectDotsChoiceSecrets(in.Options); err != nil {
		return Task{}, err
	}
	if len(in.DocKeys) > maxDotsRequestDocs {
		return Task{}, dotsRequestInvalid("docs must name at most 20 document keys")
	}
	for _, key := range in.DocKeys {
		if !validDocKey(key) {
			return Task{}, dotsRequestInvalid("docs must name document keys of 1-512 bytes without \"..\" or a leading /")
		}
	}
	if _, invalid := dotsNotifyLanes(in.Body, lane); len(invalid) > 0 {
		return Task{}, dotsRequestInvalid("the [알릴 레인] line names a lane that is not [A-Za-z0-9._-]{1,128}")
	}
	var project *string
	if in.Project != "" {
		project = &in.Project
	}
	item := triageItem{Title: in.Title, Body: dotsRequestBody(in), Kind: "verify", Project: project}
	if err := checkTriageItem(item); err != nil {
		// The bounds are the intake validator's; a secret-like title or body
		// keeps guard's own refusal.
		if _, secret := guard.SecretPattern(err); secret {
			return Task{}, err
		}
		return Task{}, dotsRequestInvalid(strings.TrimPrefix(err.Error(), "invalid intake: "))
	}
	digestParts := []string{in.Title, in.Body, lane, in.Project}
	if in.Type != "" {
		digestParts = append(digestParts, in.Type)
	}
	if len(in.Options) > 0 {
		encoded, err := json.Marshal(in.Options)
		if err != nil {
			return Task{}, err
		}
		digestParts = append(digestParts, string(encoded))
	}
	if len(in.DocKeys) > 0 {
		// Only a request that cites documents adds this part, so the digest of
		// every request without one is unchanged.
		digestParts = append(digestParts, strings.Join(in.DocKeys, "\x01"))
	}
	requestHash := sha256.Sum256([]byte(strings.Join(digestParts, "\x00")))
	requestDigest := hex.EncodeToString(requestHash[:])
	var sourceRef string
	if in.IdempotencyKey != "" {
		if !validSourceRef(in.IdempotencyKey) {
			return Task{}, dotsRequestInvalid("idempotency key must be 1-512 bytes without control characters")
		}
		if err := guard.Reject(in.IdempotencyKey); err != nil {
			return Task{}, err
		}
		sum := sha256.Sum256([]byte(by + "\x00" + in.IdempotencyKey))
		sourceRef = "hk-" + hex.EncodeToString(sum[:])
	} else {
		var key [16]byte
		if _, err := rand.Read(key[:]); err != nil {
			return Task{}, err
		}
		sourceRef = "hk-" + hex.EncodeToString(key[:])
	}
	var out Task
	err := s.retryTx(ctx, func() error {
		out = Task{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
			return err
		}
		if in.IdempotencyKey != "" {
			existing, found, err := taskBySourceTx(ctx, tx, "dots", sourceRef)
			if err != nil {
				return err
			}
			if found {
				var original string
				if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT note FROM task_events WHERE task_id=$1 AND source='dots-request' AND source_ref=$2`, `SELECT note FROM task_events WHERE task_id=? AND source='dots-request' AND source_ref=?`), existing.ID, sourceRef).Scan(&original); err != nil {
					return err
				}
				if original != requestDigest {
					return dotsRequestInvalid("idempotency key was already used with different input")
				}
				out = existing
				return nil
			}
		}
		x, err := insertTriageTaskTx(ctx, tx, item, "dots", sourceRef, "dots", nil, by, time.Now().UTC())
		if err != nil {
			return err
		}
		// A declared option names pictures and videos this database holds; the
		// check runs in the writing transaction, before the task is returned, so a
		// request can never declare an option the operator cannot look at.
		if s.dialect == mysqlDialect {
			err = checkDecisionAttachmentsExistMySQL(ctx, tx, DecisionOptions{Options: in.Options})
		} else {
			err = checkDecisionAttachmentsExist(ctx, pgxTx(tx), DecisionOptions{Options: in.Options})
		}
		if err != nil {
			return err
		}
		if err := s.enqueueYTIssueTx(ctx, tx, x); err != nil {
			return err
		}
		if err = s.updateTaskReturningCore(ctx, tx, &x, x.ID, `UPDATE tasks SET state=$2,parent_lane=$3,started_triage_at=NULL WHERE id=$1 RETURNING `, []any{x.ID, StateBacklog, lane},
			`UPDATE tasks SET state=?,parent_lane=?,started_triage_at=NULL WHERE id=?`, []any{StateBacklog, lane, x.ID}); err != nil {
			return err
		}
		if len(in.Options) > 0 {
			declared, err := json.Marshal(DotsChoice{Options: in.Options})
			if err != nil {
				return err
			}
			if err := s.updateTaskReturningCore(ctx, tx, &x, x.ID, `UPDATE tasks SET refs=jsonb_set(refs,'{dots_choice}',$2::jsonb) WHERE id=$1 RETURNING `, []any{x.ID, string(declared)},
				`UPDATE tasks SET refs=JSON_SET(refs,'$.dots_choice',CAST(? AS JSON)) WHERE id=?`, []any{string(declared), x.ID}); err != nil {
				return err
			}
		}
		if in.IdempotencyKey != "" {
			if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "source_event", By: by, Note: requestDigest, At: x.CreatedAt, Source: "dots-request", SourceRef: sourceRef}); err != nil {
				return err
			}
		}
		// A cited document that is not stored refuses the whole request: the
		// transaction rolls back, so no half-recorded request is left behind.
		for _, key := range in.DocKeys {
			if _, err := addTaskDocRefTx(ctx, tx, x.ID, key, by, x.CreatedAt); err != nil {
				return err
			}
		}
		out = x
		return tx.Commit(ctx)
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// IsHKDotsRequest identifies a request that must remain on hk.
func IsHKDotsRequest(task Task) bool {
	return task.Source == "dots" && strings.HasPrefix(task.SourceRef, "hk-")
}

// DotsInbox lists open hk requests in creation order after a task cursor.
func (s *Store) DotsInbox(ctx context.Context, after int64, limit int) ([]Task, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid dots inbox cursor or limit")
	}
	rows, err := s.db().Query(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE lane='dots' AND source='dots' AND source_ref LIKE 'hk-%' AND state NOT IN (`+settledStatesSQL+`) AND ($1::bigint=0 OR (created_at,id) > (SELECT created_at,id FROM tasks WHERE id=$2)) ORDER BY created_at,id LIMIT $3`, `SELECT `+taskColumns+` FROM tasks WHERE lane='dots' AND source='dots' AND source_ref LIKE 'hk-%' AND state NOT IN (`+settledStatesSQL+`) AND (?=0 OR (created_at,id) > (SELECT created_at,id FROM tasks WHERE id=?)) ORDER BY created_at,id LIMIT ?`), after, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		if err := scanTask(rows, &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// DotsReplyInput is a structured reply to one hk request.
type DotsReplyInput struct {
	TaskID            int64      `json:"task_id"`
	Kind              string     `json:"kind"`
	Body              string     `json:"body"`
	IdempotencyKey    string     `json:"idempotency_key"`
	AskOperator       bool       `json:"ask_operator,omitempty"`
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at,omitempty"`
	// RunToken is required on every kind but START, which issues one.
	RunToken string `json:"run_token,omitempty"`
	// Takeover, on START only, starts a request that is already started: it
	// supersedes the active run token and records the handover in the
	// request's history. The old run's later replies are then refused.
	Takeover bool `json:"takeover,omitempty"`
	// Recommended names a declared option key on a RESULT: hk marks it
	// recommended on the operator's decision. RecommendationReason is dots's
	// account of why, Unverified lists what dots could not check, and OneFix
	// is its one suggested fix (hk 4789). All four are RESULT only.
	Recommended          string   `json:"recommended,omitempty"`
	RecommendationReason string   `json:"recommendation_reason,omitempty"`
	Unverified           []string `json:"unverified,omitempty"`
	OneFix               string   `json:"one_fix,omitempty"`
}

type DotsReplyResult struct {
	TaskID      int64  `json:"task_id"`
	State       string `json:"state"`
	DocumentKey string `json:"document_key,omitempty"`
	Duplicate   bool   `json:"duplicate"`
	// RunToken is set on a START result only: the token every later reply on
	// this run must carry. A START replay returns the token its first call
	// issued, which a takeover may since have superseded.
	RunToken string `json:"run_token,omitempty"`
	// InvalidNotifyLanes names the "[알릴 레인]" entries of the request body that
	// are not valid lanes. Those lanes were not notified; the reply is recorded.
	InvalidNotifyLanes []string `json:"invalid_notify_lanes,omitempty"`
}

// ErrDotsReplyKeyConflict rejects an idempotency key used for another reply.
var ErrDotsReplyKeyConflict = errors.New("dots_reply_key_conflict")

// DotsReplyAnswer is the requester lane's answer to a dots QUESTION.
type DotsReplyAnswer struct {
	// CommentID is the task comment holding the answer. It is 0 when an
	// operator closed the decision request instead.
	CommentID int64     `json:"comment_id,omitempty"`
	Text      string    `json:"text,omitempty"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// DotsReply is an attributed reply with its structured kind.
type DotsReply struct {
	TaskComment
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	// Answer is set on a QUESTION that has been answered; a QUESTION without
	// it is still open.
	Answer *DotsReplyAnswer `json:"answer,omitempty"`
}

// ListDotsReplies returns recorded replies in comment order, each QUESTION
// carrying its answer when it has one.
func (s *Store) ListDotsReplies(ctx context.Context, taskID int64) ([]DotsReply, error) {
	rows, err := s.db().Query(ctx, s.q(`SELECT c.id,c.task_id,c.body,c.author,c.created_at,e.from_value,
			dr.answered_at,COALESCE(dr.answered_by,''),COALESCE(dr.answer_comment_id,0),COALESCE(ac.body,dr.answer_text,'')
		FROM task_comments c
		JOIN task_events e ON e.task_id=c.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.to_value=c.id::text
		LEFT JOIN dots_results dr ON dr.task_id=c.task_id AND dr.comment_id=e.source_ref AND dr.kind='QUESTION'
		LEFT JOIN task_comments ac ON ac.id=dr.answer_comment_id
		WHERE c.task_id=$1 ORDER BY c.id`, `SELECT c.id,c.task_id,c.body,c.author,c.created_at,e.from_value,
			dr.answered_at,COALESCE(dr.answered_by,''),COALESCE(dr.answer_comment_id,0),COALESCE(ac.body,dr.answer_text,'')
		FROM task_comments c
		JOIN task_events e ON e.task_id=c.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.to_value=CAST(c.id AS CHAR) COLLATE utf8mb4_0900_bin
		LEFT JOIN dots_results dr ON dr.task_id=c.task_id AND dr.comment_id=e.source_ref AND dr.kind='QUESTION'
		LEFT JOIN task_comments ac ON ac.id=dr.answer_comment_id
		WHERE c.task_id=? ORDER BY c.id`), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []DotsReply{}
	for rows.Next() {
		var reply DotsReply
		var answeredAt *time.Time
		var answeredBy string
		var answerCommentID int64
		var answerText string
		if err := rows.Scan(&reply.ID, &reply.TaskID, &reply.Body, &reply.Author, &reply.CreatedAt, &reply.Kind, &answeredAt, &answeredBy, &answerCommentID, &answerText); err != nil {
			return nil, err
		}
		reply.Channel = "chatgpt"
		if answeredAt != nil {
			reply.Answer = &DotsReplyAnswer{CommentID: answerCommentID, Text: answerText, By: answeredBy, At: *answeredAt}
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}

// ReplyDots writes the reply, its state change, and its recorded outputs together.
// Every reply but START must carry the run token START issued (dots_run_token.go).
func (s *Store) ReplyDots(ctx context.Context, actor, channel string, in DotsReplyInput) (DotsReplyResult, error) {
	if in.TaskID < 1 || !validText(actor, 128) || actor == "" || channel != "chatgpt" || !validSourceRef(in.IdempotencyKey) || (in.AskOperator && in.Kind != "QUESTION") || (in.Takeover && in.Kind != "START") {
		return DotsReplyResult{}, errors.New("invalid dots reply")
	}
	if in.Kind != "START" && in.Kind != "PROGRESS" && !DotsKindRecorded(in.Kind) {
		return DotsReplyResult{}, errors.New("invalid dots reply kind")
	}
	annotated := in.Recommended != "" || in.RecommendationReason != "" || len(in.Unverified) > 0 || in.OneFix != ""
	if annotated && in.Kind != "RESULT" {
		return DotsReplyResult{}, errors.New("invalid dots reply: recommended, recommendation_reason, unverified and one_fix apply to RESULT only")
	}
	if annotated {
		if err := checkDotsReplyAnnotations(in); err != nil {
			return DotsReplyResult{}, err
		}
	}
	if in.Kind == "START" && (in.ExpectedUpdatedAt == nil || in.ExpectedUpdatedAt.IsZero()) {
		return DotsReplyResult{}, errors.New("START requires expected_updated_at")
	}
	// Refused before any state is read: a reply with no run token, or a token
	// of the wrong shape, cannot match this run.
	if in.Kind != "START" && in.RunToken == "" {
		return DotsReplyResult{}, ErrDotsRunTokenRequired
	}
	if in.Kind != "START" && !validDotsRunToken(in.RunToken) {
		return DotsReplyResult{}, ErrDotsRunTokenMismatch
	}
	if err := CheckTaskCommentBody(in.Body); err != nil {
		return DotsReplyResult{}, err
	}
	if err := guard.Reject(in.IdempotencyKey); err != nil {
		return DotsReplyResult{}, err
	}
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", in.TaskID, in.IdempotencyKey)))
	commentID := hex.EncodeToString(keyHash[:])
	expected := ""
	if in.Kind == "START" && in.ExpectedUpdatedAt != nil {
		expected = in.ExpectedUpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	payloadSource := fmt.Sprintf("%s\x00%s\x00%t\x00%s\x00%s\x00%s", in.Kind, in.Body, in.AskOperator, actor, channel, expected)
	if annotated {
		// Only an annotated reply hash covers the annotations: a reply
		// recorded before they existed keeps its payload and still replays.
		payloadSource += "\x00" + in.Recommended + "\x00" + in.RecommendationReason + "\x00" + strings.Join(in.Unverified, "\x1f") + "\x00" + in.OneFix
	}
	if in.Takeover {
		// A takeover is not the same reply as a plain START with its key. Plain
		// START payloads keep their old hash, so stored replays still match.
		payloadSource += "\x00takeover"
	}
	payloadHash := sha256.Sum256([]byte(payloadSource))
	payload := hex.EncodeToString(payloadHash[:])
	var out DotsReplyResult
	err := s.retryTx(ctx, func() error {
		out = DotsReplyResult{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		// Lock order: mutation lock, comment lock, then task row.
		if err = s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
			return err
		}
		if err = s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
			return err
		}
		res, afterCommit, err := s.replyDotsTx(ctx, tx, actor, channel, in, commentID, payload, annotated)
		if err != nil {
			return err
		}
		out = res
		if afterCommit == nil {
			// A replay recorded nothing; the transaction rolls back.
			return nil
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		afterCommit()
		return nil
	})
	if err != nil {
		return DotsReplyResult{}, err
	}
	return out, nil
}

// replyDotsTx records one reply inside the caller's transaction, which holds
// the mutation and comment locks. The returned function is the work that
// follows a commit; it is nil for a replay, which writes nothing.
func (s *Store) replyDotsTx(ctx context.Context, tx transaction, actor, channel string, in DotsReplyInput, commentID, payload string, annotated bool) (DotsReplyResult, func(), error) {
	var err error
	var task Task
	if err = scanTask(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), in.TaskID), &task); err != nil {
		if errors.Is(err, ErrNoRows) {
			return DotsReplyResult{}, nil, ErrTaskNotFound
		}
		return DotsReplyResult{}, nil, err
	}
	// The token is checked before the replay lookup: a replay returns the first
	// result, so a caller without the run must not learn it.
	if in.Kind != "START" {
		if err = checkDotsRunTokenTx(ctx, tx, task, in.RunToken); err != nil {
			return DotsReplyResult{}, nil, err
		}
	}
	var previous string
	err = tx.QueryRow(ctx, sqlFor(tx, `SELECT note FROM task_events WHERE task_id=$1 AND source='dots-hk-chatgpt' AND source_ref=$2`, `SELECT note FROM task_events WHERE task_id=? AND source='dots-hk-chatgpt' AND source_ref=?`), in.TaskID, commentID).Scan(&previous)
	if err == nil {
		// payload state document [skipped notify lanes]: the last field is
		// absent on replies recorded before it was kept.
		parts := strings.SplitN(previous, " ", 4)
		if len(parts) < 3 {
			return DotsReplyResult{}, nil, errors.New("invalid recorded dots reply")
		}
		if parts[0] != payload {
			return DotsReplyResult{}, nil, ErrDotsReplyKeyConflict
		}
		replayToken := ""
		if in.Kind == "START" {
			if replayToken, err = dotsRunTokenForIssueTx(ctx, tx, task.ID, commentID); err != nil {
				return DotsReplyResult{}, nil, err
			}
		}
		var skipped []string
		if len(parts) == 4 {
			skipped = dotsSkippedLanesFromNote(parts[3])
		}
		return DotsReplyResult{TaskID: task.ID, State: parts[1], DocumentKey: parts[2], Duplicate: true, RunToken: replayToken, InvalidNotifyLanes: skipped}, nil, nil
	}
	if !errors.Is(err, ErrNoRows) {
		return DotsReplyResult{}, nil, err
	}
	if task.Lane != "dots" || !IsHKDotsRequest(task) {
		return DotsReplyResult{}, nil, errors.New("invalid dots request task")
	}
	if in.Kind == "START" && !task.UpdatedAt.Equal(*in.ExpectedUpdatedAt) {
		return DotsReplyResult{}, nil, fmt.Errorf("%w: dots request has changed or is already claimed", ErrTaskConflict)
	}
	if in.Kind == "START" && in.Takeover {
		// A takeover starts a request that is already started: it needs a
		// claim to take over and an unsettled request to take it on.
		if task.ClaimedBy == "" || IsSettledState(task.State) {
			return DotsReplyResult{}, nil, fmt.Errorf("%w: dots request is not started, so there is no run to take over", ErrTaskConflict)
		}
	} else if in.Kind == "START" && (!IsClaimableState(task.State) || task.ClaimedBy != "") {
		return DotsReplyResult{}, nil, fmt.Errorf("%w: dots request has changed or is already claimed", ErrTaskConflict)
	}
	if in.Kind != "START" && IsSettledState(task.State) {
		return DotsReplyResult{}, nil, ErrTaskConflict
	}
	now := time.Now().UTC()
	var commentRowID int64
	if s.dialect == mysqlDialect {
		commentRowID, err = tx.InsertID(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES(?,?,?,?)`, in.TaskID, in.Body, actor, now)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING id`, in.TaskID, in.Body, actor, now).Scan(&commentRowID)
	}
	if err != nil {
		return DotsReplyResult{}, nil, err
	}
	// While comments are canonical in YouTrack (hk 4122) the reply gets its
	// op in this transaction, or is refused; under hk nothing changes.
	reply := TaskComment{ID: commentRowID, TaskID: in.TaskID, Body: in.Body, Author: actor, CreatedAt: now}
	replyOp, replyEnqueued, err := s.enqueueYTCommentIfCanonicalTx(ctx, tx, reply)
	if err != nil {
		return DotsReplyResult{}, nil, err
	}

	var docKey string
	var invalidLanes []string
	if in.Kind != "START" {
		issue := DotsIssue{Identifier: "hk-" + strconv.FormatInt(task.ID, 10), Title: task.Title, URL: fmt.Sprintf("hk:task/%d", task.ID), IssueUUID: fmt.Sprintf("hk:%d", task.ID)}
		comment := DotsComment{ID: commentID, CreatedAt: now.Format(time.RFC3339Nano), Body: "[dots] " + in.Kind + "\n" + in.Body}
		if _, ok := DotsKind(comment.Body); !ok {
			return DotsReplyResult{}, nil, errors.New("invalid dots reply")
		}
		// The body document is editable after creation, so a bad notify line
		// cannot refuse the reply (that would roll back the RESULT). It is
		// skipped and reported in the result instead.
		var notifyLanes []string
		if in.Kind == "RESULT" {
			if notifyLanes, invalidLanes, err = dotsNotifyLanesFlowTx(ctx, tx, task); err != nil {
				return DotsReplyResult{}, nil, err
			}
		}
		var askOperator *bool
		if in.Kind == "QUESTION" {
			ask := in.AskOperator
			askOperator = &ask
		}
		docKey, _, err = recordDotsResultFlowTx(ctx, tx, in.Kind, issue, comment, &task, task.ParentLane, "hk-mcp", askOperator, notifyLanes...)
		if err != nil {
			return DotsReplyResult{}, nil, err
		}
	}
	var runToken string
	switch in.Kind {
	case "START":
		if in.Takeover {
			runToken, err = issueDotsRunTokenTx(ctx, tx, task.ID, commentID, actor, true, now)
			break
		}
		task, err = s.claimTaskTx(ctx, tx, task.ID, "dots", "", "dots request")
		if err == nil {
			runToken, err = issueDotsRunTokenTx(ctx, tx, task.ID, commentID, actor, false, now)
		}
	case "BLOCKED":
		task, err = s.transitionTaskTx(ctx, tx, task.ID, TransitionInput{To: "hold", By: actor, Note: in.Body}, false)
	case "RESULT":
		if task.Refs.DotsChoice == nil {
			if annotated {
				err = errors.New("invalid dots reply: the request declares no options, so recommended, recommendation_reason, unverified and one_fix have no decision to carry")
				break
			}
			task, err = s.transitionTaskTx(ctx, tx, task.ID, TransitionInput{To: StateCompleted, By: actor, Evidence: "hk:doc " + docKey}, false)
			break
		}
		// A declaring request ends in the operator's choice: the RESULT opens
		// that decision, and the task waits on it instead of completing
		// (hk 4789).
		input, buildErr := dotsChoiceDecisionInput(task, docKey, in)
		if buildErr != nil {
			err = buildErr
			break
		}
		var decision DecisionRequestResult
		decision, err = s.recordDecisionRequestFlowTx(ctx, tx, task.ID, actor, input)
		if err != nil {
			break
		}
		task = decision.Task
		err = markDotsChoiceResultFlowTx(ctx, tx, task.ID, docKey)
	case "QUESTION":
		if in.AskOperator {
			var decision DecisionRequestResult
			decision, err = s.recordDecisionRequestFlowTx(ctx, tx, task.ID, actor, DecisionRequestInput{Question: in.Body, Options: DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Provide an answer"}}, AllowFree: true}, DefaultAction: "Wait for an answer", Block: true, DotsQuestionCommentID: commentRowID})
			task = decision.Task
		}
	}
	if err != nil {
		return DotsReplyResult{}, nil, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: task.ID, Kind: "source_event", By: actor, Note: payload + " " + task.State + " " + docKey + dotsSkippedLanesNote(invalidLanes), At: now, Source: "dots-hk-chatgpt", SourceRef: commentID, Field: "dots_reply", FromValue: in.Kind, ToValue: strconv.FormatInt(commentRowID, 10)}); err != nil {
		return DotsReplyResult{}, nil, err
	}
	return DotsReplyResult{TaskID: task.ID, State: task.State, DocumentKey: docKey, RunToken: runToken, InvalidNotifyLanes: invalidLanes}, func() { logYTCommentEnqueued(reply, replyOp, replyEnqueued) }, nil
}

// dotsSkippedLanesNote is the optional last field of a reply's event note: the
// notify names a RESULT skipped, escaped so the field has no space.
func dotsSkippedLanesNote(lanes []string) string {
	if len(lanes) == 0 {
		return ""
	}
	escaped := make([]string, len(lanes))
	for i, lane := range lanes {
		escaped[i] = url.PathEscape(lane)
	}
	return " " + strings.Join(escaped, ",")
}

// dotsSkippedLanesFromNote reads the field dotsSkippedLanesNote wrote.
func dotsSkippedLanesFromNote(field string) []string {
	var lanes []string
	for _, part := range strings.Split(field, ",") {
		if lane, err := url.PathUnescape(part); err == nil && lane != "" {
			lanes = append(lanes, lane)
		}
	}
	return lanes
}

// ErrNoOpenDotsQuestion refuses an answer when the request has neither an open
// decision request nor an open QUESTION reply. Its reason names what is
// missing (hk 5000).
var ErrNoOpenDotsQuestion = errors.New("no_open_dots_question")

// ErrDotsQuestionAmbiguous refuses an answer when more than one QUESTION reply
// is open and the caller named none: answering one silently would leave the
// other open with no answer (hk 5000).
var ErrDotsQuestionAmbiguous = errors.New("dots_question_ambiguous")

// DotsAnswerInput carries the text answering a task's open dots question.
type DotsAnswerInput struct {
	Text string `json:"text"`
	// RequesterLane must name the request's requester lane when the answer
	// closes a plain QUESTION, exactly as a cancel does. An open decision
	// request keeps its own responder rule and ignores it.
	RequesterLane string `json:"requester_lane,omitempty"`
	// QuestionID names the QUESTION reply to close when more than one is
	// open: the reply's id in dots_inbox (its task comment id).
	QuestionID int64 `json:"question_id,omitempty"`
}

// DotsAnsweredQuestion is the plain QUESTION reply an answer closed.
type DotsAnsweredQuestion struct {
	ResultID  int64 `json:"result_id"`
	CommentID int64 `json:"comment_id"`
}

// DotsAnswerResult is the outcome of answering a task's open dots question.
type DotsAnswerResult struct {
	TaskID    int64                 `json:"task_id"`
	RequestID string                `json:"request_id,omitempty"`
	CommentID int64                 `json:"comment_id"`
	Task      Task                  `json:"task"`
	Request   *DecisionRequest      `json:"request,omitempty"`
	Question  *DotsAnsweredQuestion `json:"question,omitempty"`
	Comment   CreatedTaskComment    `json:"comment"`
}

// AnswerDotsQuestion answers a task's open dots question and records the text
// as a task comment, in one transaction. An open decision request is resolved
// exactly as before, its responder rule unchanged. Otherwise the latest
// QUESTION reply is closed, and only the request's requester lane (parent_lane,
// as CancelDotsRequest decides it) may answer it. A QUESTION is answered once:
// the answer marks its dots_results row, so a second answer finds nothing open.
// A requester-lane comment alone answers nothing.
func (s *Store) AnswerDotsQuestion(ctx context.Context, taskID int64, caller string, in DotsAnswerInput) (DotsAnswerResult, error) {
	if taskID < 1 {
		return DotsAnswerResult{}, ErrTaskNotFound
	}
	if caller == "" || !validText(caller, 128) {
		return DotsAnswerResult{}, errors.New("caller is required")
	}
	if err := CheckTaskCommentBody(in.Text); err != nil {
		return DotsAnswerResult{}, err
	}
	if err := guard.Reject(in.Text); err != nil {
		return DotsAnswerResult{}, err
	}

	var out DotsAnswerResult
	err := s.retryTx(ctx, func() error {
		out = DotsAnswerResult{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)

		// Lock order matches ReplyDots: task-mutation
		// advisory lock (level 2), then taskCommentInsertLock, then task row FOR UPDATE.
		if err = s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
			return err
		}
		if err = s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
			return err
		}
		res, afterCommit, err := s.answerDotsQuestionTx(ctx, tx, taskID, caller, in)
		if err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		out = res
		afterCommit()
		return nil
	})
	if err != nil {
		return DotsAnswerResult{}, err
	}
	return out, nil
}

// answerDotsQuestionTx answers inside the caller's transaction, which holds the
// mutation and comment locks. The returned function is the work that follows
// the commit.
func (s *Store) answerDotsQuestionTx(ctx context.Context, tx transaction, taskID int64, caller string, in DotsAnswerInput) (DotsAnswerResult, func(), error) {
	var err error
	var x Task
	if err = scanTask(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), taskID), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return DotsAnswerResult{}, nil, ErrTaskNotFound
		}
		return DotsAnswerResult{}, nil, err
	}

	if x.Lane != "dots" || !IsHKDotsRequest(x) {
		return DotsAnswerResult{}, nil, errors.New("invalid dots request task")
	}
	if x.Refs.Disposition != nil {
		return DotsAnswerResult{}, nil, ErrDispositionOperatorOnly
	}
	if isTerminalTaskState(x.State) {
		return DotsAnswerResult{}, nil, ErrTaskTerminal
	}
	// A previous release may write a QUESTION row during blue-green with no
	// owner column; classify this task's rows before selecting one (hk 5000).
	if err = classifyDotsQuestionsFlowTx(ctx, tx, taskID); err != nil {
		return DotsAnswerResult{}, nil, err
	}

	current := x.Refs.DecisionRequest
	decisionOpen := current != nil && current.Status == DecisionRequestOpen
	// routeDecision is true when this answer resolves the open decision request:
	// an untargeted answer, or a target that is an ask_operator question. A
	// target that is a plain question closes that question and leaves the
	// decision open (hk 5000).
	var question dotsQuestion
	routeDecision := false
	switch {
	case in.QuestionID > 0:
		q, found, qerr := dotsQuestionTargetFlowTx(ctx, tx, taskID, in.QuestionID)
		if qerr != nil {
			return DotsAnswerResult{}, nil, qerr
		}
		if !found {
			return DotsAnswerResult{}, nil, fmt.Errorf("%w: no open dots question has the given question_id", ErrNoOpenDotsQuestion)
		}
		question = q
		if q.AskOperator {
			if !decisionOpen {
				return DotsAnswerResult{}, nil, ErrDecisionRequestResolved
			}
			// The target must be the open request's own question. A
			// superseded request's rows are closed, and anything else is
			// refused rather than resolving the wrong decision (hk 5000).
			if current.DotsQuestionCommentID == 0 || q.CommentID != current.DotsQuestionCommentID {
				return DotsAnswerResult{}, nil, fmt.Errorf("%w: the given question is not the open decision request's question", ErrNoOpenDotsQuestion)
			}
			routeDecision = true
		}
	case decisionOpen:
		// The untargeted answer resolves the decision, as before.
		routeDecision = true
	default:
		questions, qerr := dotsPlainQuestionsFlowTx(ctx, tx, taskID)
		if qerr != nil {
			return DotsAnswerResult{}, nil, qerr
		}
		switch len(questions) {
		case 0:
			// What was last asked tells the refusal apart: a decision question
			// (already closed) means this is a second answer, otherwise nothing
			// is open and the reason says so.
			ask, found, qerr := dotsLatestQuestionAskOperatorFlowTx(ctx, tx, taskID)
			if qerr != nil {
				return DotsAnswerResult{}, nil, qerr
			}
			if found && ask {
				return DotsAnswerResult{}, nil, ErrDecisionRequestResolved
			}
			return DotsAnswerResult{}, nil, fmt.Errorf("%w: this request has no open dots question or decision request to answer", ErrNoOpenDotsQuestion)
		case 1:
			question = questions[0]
		default:
			return DotsAnswerResult{}, nil, fmt.Errorf("%w: more than one dots question is open; name the one to answer with question_id", ErrDotsQuestionAmbiguous)
		}
	}
	if !routeDecision {
		lane := strings.TrimSpace(in.RequesterLane)
		if lane == "" || !validName(lane) {
			return DotsAnswerResult{}, nil, dotsRequestInvalid("answering a plain dots question requires the requester lane")
		}
		if x.ParentLane != lane {
			return DotsAnswerResult{}, nil, fmt.Errorf("%w: only the request's requester lane may answer its question", ErrTaskConflict)
		}

		now := time.Now().UTC()
		var comment TaskComment
		err = insertDotsAnswerCommentTx(ctx, tx, &comment, taskID, in.Text, caller, now)
		if err != nil {
			return DotsAnswerResult{}, nil, err
		}
		op, enqueued, err := s.enqueueYTCommentTx(ctx, tx, comment)
		if err != nil {
			return DotsAnswerResult{}, nil, err
		}
		if err = markDotsQuestionAnsweredFlowTx(ctx, tx, question.ResultID, caller, comment.ID, now); err != nil {
			return DotsAnswerResult{}, nil, err
		}
		projection := ytProjectionOf(op, enqueued)
		return DotsAnswerResult{
			TaskID:    taskID,
			CommentID: comment.ID,
			Task:      x,
			Question:  &DotsAnsweredQuestion{ResultID: question.ResultID, CommentID: question.CommentID},
			Comment:   CreatedTaskComment{TaskComment: comment, YouTrackProjection: &projection},
		}, func() { logYTCommentEnqueued(comment, op, enqueued) }, nil
	}
	if current.EffectivePolicy(false) == DecisionPolicyOperatorDirect {
		return DotsAnswerResult{}, nil, ErrDecisionRequestHumanOnly
	}

	now := time.Now().UTC()

	// 1. Insert comment
	var comment TaskComment
	err = insertDotsAnswerCommentTx(ctx, tx, &comment, taskID, in.Text, caller, now)
	if err != nil {
		return DotsAnswerResult{}, nil, err
	}
	op, enqueued, err := s.enqueueYTCommentTx(ctx, tx, comment)
	if err != nil {
		return DotsAnswerResult{}, nil, err
	}
	// Closing the decision closes its ask_operator questions (display only).
	if err = s.closeDotsAskOperatorQuestionsFlowTx(ctx, tx, taskID, caller, in.Text, comment.ID, now); err != nil {
		return DotsAnswerResult{}, nil, err
	}

	// 2. Resolve decision request
	decisionBefore := decisionBeforeOf(x)
	root := current.RootID()
	eventKey := fmt.Sprintf("auto:dots-answer:%d:%s", taskID, current.ID)

	attempt := decisionAttempt{
		channel:           DecisionChannelCLI,
		eventKey:          eventKey,
		taskID:            taskID,
		revisionRequestID: current.ID,
		revision:          current.Revision,
		root:              root,
		provenance:        DecisionProvenanceOperatorRelay,
		actor:             caller,
		at:                now,
		text:              in.Text,
		result:            DecisionResultAnswered,
	}
	attemptID, err := insertDecisionAttemptTx(ctx, tx, attempt)
	if err != nil {
		return DotsAnswerResult{}, nil, err
	}

	resolved := *current
	resolved.Status = DecisionRequestAnswered
	resolved.Resolution = &DecisionResolution{
		Kind:      DecisionRequestAnswered,
		Text:      in.Text,
		Responder: caller,
		By:        caller,
		At:        now,
	}
	x.Refs.DecisionRequest = &resolved

	flagWas := x.AwaitingDecision
	if flagWas {
		var others int
		if others, err = otherBlockingDecisionsFlowTx(ctx, tx, x.ID, resolved.ID); err != nil {
			return DotsAnswerResult{}, nil, err
		}
		x.AwaitingDecision = others > 0
	}

	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return DotsAnswerResult{}, nil, err
	}
	if err = s.updateTaskReturningCore(ctx, tx, &x, x.ID, `UPDATE tasks SET refs=$2::jsonb,awaiting_decision=$4,updated_at=$3 WHERE id=$1 RETURNING `, []any{x.ID, string(encoded), now, x.AwaitingDecision},
		`UPDATE tasks SET refs=?,awaiting_decision=?,updated_at=? WHERE id=?`, []any{string(encoded), x.AwaitingDecision, now, x.ID}); err != nil {
		return DotsAnswerResult{}, nil, err
	}

	resolutionID, err := tx.InsertID(ctx, s.q(`INSERT INTO decision_resolutions(request_id,question_revision,revision_request_id,task_id,kind,option_id,text,receipt,provenance,channel,decided_by,recorded_by,attempt_id,committed_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,$12,$13,$14) RETURNING resolution_id`,
		`INSERT INTO decision_resolutions(request_id,question_revision,revision_request_id,task_id,kind,option_id,text,receipt,provenance,channel,decided_by,recorded_by,attempt_id,committed_at)
		VALUES(?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?)`),
		root, current.Revision, resolved.ID, x.ID, DecisionRequestAnswered, "", in.Text, "", DecisionProvenanceOperatorRelay, DecisionChannelCLI,
		caller, caller, attemptID, now)
	if err != nil {
		return DotsAnswerResult{}, nil, err
	}

	note := "decision-" + DecisionRequestAnswered + " " + resolved.ID + ": " + in.Text
	resolveIn := ResolveDecisionInput{
		TaskID:    taskID,
		RequestID: current.ID,
		Kind:      DecisionRequestAnswered,
		Text:      in.Text,
		Responder: caller,
		Channel:   DecisionChannelCLI,
		Actor:     caller,
	}
	if err = s.queueDecisionAnswerNoticeFlow(ctx, tx, x, resolved, resolveIn, caller, resolutionID, now); err != nil {
		return DotsAnswerResult{}, nil, err
	}
	if flagWas && !x.AwaitingDecision {
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: TaskEventAwaiting, From: x.State, To: x.State, By: caller, Note: "decision-clear " + resolved.ID + " by " + caller, Refs: x.Refs, Into: &x, At: now, Field: "awaiting_decision", FromValue: "true", ToValue: "false"}); err != nil {
			return DotsAnswerResult{}, nil, err
		}
	}
	if err = insertDecisionEventTx(ctx, tx, &x, caller, note, now); err != nil {
		return DotsAnswerResult{}, nil, err
	}
	if err = s.mirrorTaskDecisionsFlowTx(ctx, tx, decisionBefore, x, caller, note, 0, now); err != nil {
		return DotsAnswerResult{}, nil, err
	}
	// A declaring dots request reconciles its result on the answer (hk 4789).
	if x, err = s.reconcileDotsChoiceTx(ctx, tx, x, now); err != nil {
		return DotsAnswerResult{}, nil, err
	}

	projection := ytProjectionOf(op, enqueued)

	return DotsAnswerResult{
		TaskID:    taskID,
		RequestID: resolved.ID,
		CommentID: comment.ID,
		Task:      x,
		Request:   &resolved,
		Comment:   CreatedTaskComment{TaskComment: comment, YouTrackProjection: &projection},
	}, func() { logYTCommentEnqueued(comment, op, enqueued) }, nil
}

// DotsComment is one Linear comment. CreatedAt keeps Linear's own ISO-8601
// text: the document key takes its date from that text, not from a parsed and
// re-rendered time.
type DotsComment struct {
	ID        string
	CreatedAt string
	Body      string
}

// DotsKind classifies a comment. dots is false when the comment does not start
// with "[dots]"; a bare "[dots]" heading is kind "?", which is never recorded.
// The kind word ends at a colon, so "[dots] RESULT: <verdict>" is a RESULT
// (ROB-1497 on 2026-11-05 was dropped as kind "RESULT:").
func DotsKind(body string) (kind string, dots bool) {
	word, _, dots := dotsHeading(body)
	if !dots {
		return "", false
	}
	if word == "" {
		return "?", true
	}
	return strings.ToUpper(word), true
}

// dotsHeading splits the first line after "[dots]" into the kind word and,
// when that word ends at a colon, the verdict that follows it on the line.
func dotsHeading(body string) (word, verdict string, dots bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	rest, ok := strings.CutPrefix(head, "[dots]")
	if !ok {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	words := strings.Fields(rest)
	if len(words) == 0 {
		return "", "", true
	}
	word = words[0]
	if i := strings.IndexAny(word, ":："); i >= 0 {
		_, size := utf8.DecodeRuneInString(word[i:])
		verdict = strings.TrimSpace(rest[i+size:])
		word = word[:i]
	}
	return word, verdict, true
}

// DotsKindRecorded reports whether a comment of this kind becomes a document.
func DotsKindRecorded(kind string) bool {
	_, ok := dotsResultKinds[kind]
	return ok
}

// firstRunes is Python's s[:n]: n code points, not n bytes.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// dotsSummaryLine is the verdict on a "[dots] KIND: <verdict>" heading, else
// the first substantive line after the heading, skipping a "결론" heading and
// table rows.
func dotsSummaryLine(body string) string {
	if _, verdict, _ := dotsHeading(body); verdict != "" {
		return firstRunes(verdict, 220)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for _, line := range lines[1:] {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if s != "" && s != "결론" && !strings.HasPrefix(s, "|") {
			return firstRunes(s, 220)
		}
	}
	return ""
}

func dotsResultKey(issue DotsIssue, c DotsComment) string {
	return "dots/results/" + issue.Identifier + "/" + c.CreatedAt[:10] + "-" + c.ID[:8]
}

func dotsResultBody(kind string, issue DotsIssue, c DotsComment) string {
	return "# [dots] " + kind + " " + issue.Identifier + " — " + issue.Title + "\n\n" + issue.URL + "\n\n" + c.Body + "\n"
}

// dotsLaneText replaces controls that the hub rejects while retaining the
// full event text.
func dotsLaneText(text string) string {
	return assistantLaneText("", strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, text))
}

func dotsResultText(kind, key string, issue DotsIssue, c DotsComment) string {
	return dotsLaneText("[dots] " + kind + " " + issue.Identifier + " " + firstRunes(issue.Title, 60) + " :: " + dotsSummaryLine(c.Body) + " → hk:doc " + key + " · " + issue.URL)
}

func checkDotsResult(kind string, issue DotsIssue, c DotsComment, allowProgress bool) error {
	if !DotsKindRecorded(kind) && !(allowProgress && kind == "PROGRESS") {
		return errors.New("invalid dots result")
	}
	if len(c.ID) < 8 {
		return errors.New("invalid dots result")
	}
	if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
		return errors.New("invalid dots result")
	}
	key := dotsResultKey(issue, c)
	body := dotsResultBody(kind, issue, c)
	if !validDocKey(key) || !validDocumentText(body) {
		return errors.New("invalid dots result")
	}
	return guard.Reject(key + "\n" + body)
}

// dotsNotifyLanes splits the lanes a request body names with a "[알릴 레인]" line
// into valid lanes (other than the requester) and invalid names. CreateDotsRequest
// refuses a body with an invalid name; a reply skips and reports it, because a
// body document can be edited after creation.
func dotsNotifyLanes(body, requester string) (lanes, invalid []string) {
	seen := map[string]bool{requester: true}
	for _, m := range dotsNotifyLaneLineRE.FindAllStringSubmatch(body, -1) {
		for _, name := range strings.Split(m[1], ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if validName(name) {
				lanes = append(lanes, name)
			} else {
				invalid = append(invalid, name)
			}
		}
	}
	return lanes, invalid
}

// dotsNotifyLanesTx reads the body of a dots request and returns its valid and
// invalid notify lanes. A body_doc may carry a "#section" suffix (ValidBodyDoc
// admits it), so the document key is the part before the "#".
func dotsNotifyLanesTx(ctx context.Context, tx pgx.Tx, task Task) (lanes, invalid []string, err error) {
	return dotsNotifyLanesFlowTx(ctx, pgTransaction{tx}, task)
}

// dotsNotifyLanesFlowTx is dotsNotifyLanesTx on either dialect.
func dotsNotifyLanesFlowTx(ctx context.Context, tx transaction, task Task) (lanes, invalid []string, err error) {
	if task.BodyDoc == "" {
		return nil, nil, nil
	}
	key, _, _ := strings.Cut(task.BodyDoc, "#")
	var body string
	if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT body FROM documents WHERE key=$1`, "SELECT body FROM documents WHERE `key`=?"), key).Scan(&body); err != nil {
		if errors.Is(err, ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	lanes, invalid = dotsNotifyLanes(body, task.ParentLane)
	return lanes, invalid, nil
}

// recordDotsResultTx records a dots comment and its document. Only a RESULT
// or a QUESTION or BLOCKED reaches an outbox row (lane event); PROGRESS is
// kept as a document and sends nothing. notifyLanes add one lane event each,
// beside the requester lane, for a RESULT.
func recordDotsResultTx(ctx context.Context, tx pgx.Tx, kind string, issue DotsIssue, c DotsComment, task *Task, lane, createdBy string, askOperator *bool, notifyLanes ...string) (key string, created bool, err error) {
	return recordDotsResultFlowTx(ctx, pgTransaction{tx}, kind, issue, c, task, lane, createdBy, askOperator, notifyLanes...)
}

// recordDotsResultFlowTx is recordDotsResultTx on either dialect.
func recordDotsResultFlowTx(ctx context.Context, tx transaction, kind string, issue DotsIssue, c DotsComment, task *Task, lane, createdBy string, askOperator *bool, notifyLanes ...string) (key string, created bool, err error) {
	progress := kind == "PROGRESS" && createdBy == "hk-mcp"
	if err := checkDotsResult(kind, issue, c, progress); err != nil {
		return "", false, err
	}
	outboxKind := dotsResultKinds[kind]
	if progress {
		outboxKind = OutboxKindDotsReport
	}
	key = dotsResultKey(issue, c)
	body := dotsResultBody(kind, issue, c)
	sum := sha256.Sum256([]byte(body))
	artefact := sha256.Sum256([]byte(c.Body))
	verdict := nullIfEmpty(dotsSummaryLine(c.Body))
	now := time.Now().UTC()
	var taskID *int64
	if task != nil {
		taskID = &task.ID
	}
	created, err = insertDotsDocumentTx(ctx, tx, key, body, hex.EncodeToString(sum[:]), createdBy, now, taskID)
	if err != nil {
		return "", false, err
	}
	if createdBy == "hk-mcp" && !created {
		return "", false, errors.New("dots reply document key collision")
	}
	if !created && taskID != nil {
		if _, err := tx.Exec(ctx, sqlFor(tx, `UPDATE documents SET task_id=$1 WHERE key=$2 AND task_id IS NULL`, "UPDATE documents SET task_id=? WHERE `key`=? AND task_id IS NULL"), *taskID, key); err != nil {
			return "", false, err
		}
	}
	if !progress {
		if err := upsertDotsResultTx(ctx, tx, key, c.ID, issue, kind, verdict, hex.EncodeToString(artefact[:]), taskID, lane, askOperator, now); err != nil {
			return "", false, err
		}
	}
	if created && !progress {
		text := dotsResultText(kind, key, issue, c)
		if err := insertNotificationOutbox(ctx, tx, outboxKind, lane, "dots-"+c.ID, text, now); err != nil {
			return "", false, err
		}
		for _, extra := range notifyLanes {
			if err := insertNotificationOutbox(ctx, tx, outboxKind, extra, "dots-"+c.ID+"-"+extra, text, now); err != nil {
				return "", false, err
			}
		}
	}
	return key, created, nil
}

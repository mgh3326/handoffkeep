package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
}

// CreateDotsRequest creates a standalone request and its body in one transaction.
func (s *Store) CreateDotsRequest(ctx context.Context, by string, in DotsRequestInput) (Task, error) {
	lane := in.RequesterLane
	if lane == "" {
		lane = by
	}
	if !validName(lane) || by == "" || !validText(by, 128) || strings.TrimSpace(in.Body) == "" {
		return Task{}, errors.New("invalid dots request")
	}
	if _, invalid := dotsNotifyLanes(in.Body, lane); len(invalid) > 0 {
		return Task{}, fmt.Errorf("invalid dots notify lane %q", invalid[0])
	}
	var project *string
	if in.Project != "" {
		project = &in.Project
	}
	item := triageItem{Title: in.Title, Body: in.Body, Kind: "verify", Project: project}
	if err := checkTriageItem(item); err != nil {
		return Task{}, err
	}
	requestHash := sha256.Sum256([]byte(strings.Join([]string{in.Title, in.Body, lane, in.Project}, "\x00")))
	requestDigest := hex.EncodeToString(requestHash[:])
	var sourceRef string
	if in.IdempotencyKey != "" {
		if !validSourceRef(in.IdempotencyKey) {
			return Task{}, errors.New("invalid dots request idempotency key")
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}
	if in.IdempotencyKey != "" {
		existing, found, err := taskBySourceTx(ctx, tx, "dots", sourceRef)
		if err != nil {
			return Task{}, err
		}
		if found {
			var original string
			if err := tx.QueryRow(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND source='dots-request' AND source_ref=$2`, existing.ID, sourceRef).Scan(&original); err != nil {
				return Task{}, err
			}
			if original != requestDigest {
				return Task{}, errors.New("dots request idempotency key reused with different input")
			}
			return existing, nil
		}
	}
	x, err := insertTriageTaskTx(ctx, tx, item, "dots", sourceRef, "dots", nil, by, time.Now().UTC())
	if err != nil {
		return Task{}, err
	}
	if err := s.enqueueYTIssueTx(ctx, tx, x); err != nil {
		return Task{}, err
	}
	err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state=$2,parent_lane=$3,started_triage_at=NULL WHERE id=$1 RETURNING `+taskColumns, x.ID, StateBacklog, lane), &x)
	if err != nil {
		return Task{}, err
	}
	if in.IdempotencyKey != "" {
		if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "source_event", By: by, Note: requestDigest, At: x.CreatedAt, Source: "dots-request", SourceRef: sourceRef}); err != nil {
			return Task{}, err
		}
	}
	return x, tx.Commit(ctx)
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
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE lane='dots' AND source='dots' AND source_ref LIKE 'hk-%' AND state NOT IN (`+settledStatesSQL+`) AND ($1::bigint=0 OR (created_at,id) > (SELECT created_at,id FROM tasks WHERE id=$1)) ORDER BY created_at,id LIMIT $2`, after, limit)
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

// DotsReply is an attributed reply with its structured kind.
type DotsReply struct {
	TaskComment
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
}

// ListDotsReplies returns recorded replies in comment order.
func (s *Store) ListDotsReplies(ctx context.Context, taskID int64) ([]DotsReply, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.task_id,c.body,c.author,c.created_at,e.from_value FROM task_comments c JOIN task_events e ON e.task_id=c.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.to_value=c.id::text WHERE c.task_id=$1 ORDER BY c.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []DotsReply{}
	for rows.Next() {
		var reply DotsReply
		if err := rows.Scan(&reply.ID, &reply.TaskID, &reply.Body, &reply.Author, &reply.CreatedAt, &reply.Kind); err != nil {
			return nil, err
		}
		reply.Channel = "chatgpt"
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
	if in.Takeover {
		// A takeover is not the same reply as a plain START with its key. Plain
		// START payloads keep their old hash, so stored replays still match.
		payloadSource += "\x00takeover"
	}
	payloadHash := sha256.Sum256([]byte(payloadSource))
	payload := hex.EncodeToString(payloadHash[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DotsReplyResult{}, err
	}
	defer tx.Rollback(ctx)
	// Lock order: mutation lock, comment lock, then task row.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return DotsReplyResult{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		return DotsReplyResult{}, err
	}
	var task Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, in.TaskID), &task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DotsReplyResult{}, ErrTaskNotFound
		}
		return DotsReplyResult{}, err
	}
	// The token is checked before the replay lookup: a replay returns the first
	// result, so a caller without the run must not learn it.
	if in.Kind != "START" {
		if err = checkDotsRunTokenTx(ctx, tx, task, in.RunToken); err != nil {
			return DotsReplyResult{}, err
		}
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND source='dots-hk-chatgpt' AND source_ref=$2`, in.TaskID, commentID).Scan(&previous)
	if err == nil {
		parts := strings.SplitN(previous, " ", 3)
		if len(parts) != 3 {
			return DotsReplyResult{}, errors.New("invalid recorded dots reply")
		}
		if parts[0] != payload {
			return DotsReplyResult{}, ErrDotsReplyKeyConflict
		}
		replayToken := ""
		if in.Kind == "START" {
			if replayToken, err = dotsRunTokenForIssueTx(ctx, tx, task.ID, commentID); err != nil {
				return DotsReplyResult{}, err
			}
		}
		return DotsReplyResult{TaskID: task.ID, State: parts[1], DocumentKey: parts[2], Duplicate: true, RunToken: replayToken}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DotsReplyResult{}, err
	}
	if task.Lane != "dots" || !IsHKDotsRequest(task) {
		return DotsReplyResult{}, errors.New("invalid dots request task")
	}
	if in.Kind == "START" && !task.UpdatedAt.Equal(*in.ExpectedUpdatedAt) {
		return DotsReplyResult{}, fmt.Errorf("%w: dots request has changed or is already claimed", ErrTaskConflict)
	}
	if in.Kind == "START" && in.Takeover {
		// A takeover starts a request that is already started: it needs a
		// claim to take over and an unsettled request to take it on.
		if task.ClaimedBy == "" || IsSettledState(task.State) {
			return DotsReplyResult{}, fmt.Errorf("%w: dots request is not started, so there is no run to take over", ErrTaskConflict)
		}
	} else if in.Kind == "START" && (!IsClaimableState(task.State) || task.ClaimedBy != "") {
		return DotsReplyResult{}, fmt.Errorf("%w: dots request has changed or is already claimed", ErrTaskConflict)
	}
	if in.Kind != "START" && IsSettledState(task.State) {
		return DotsReplyResult{}, ErrTaskConflict
	}
	now := time.Now().UTC()
	var commentRowID int64
	if err = tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING id`, in.TaskID, in.Body, actor, now).Scan(&commentRowID); err != nil {
		return DotsReplyResult{}, err
	}
	// While comments are canonical in YouTrack (hk 4122) the reply gets its
	// op in this transaction, or is refused; under hk nothing changes.
	reply := TaskComment{ID: commentRowID, TaskID: in.TaskID, Body: in.Body, Author: actor, CreatedAt: now}
	replyOp, replyEnqueued, err := s.enqueueYTCommentIfCanonicalTx(ctx, tx, reply)
	if err != nil {
		return DotsReplyResult{}, err
	}

	var docKey string
	var invalidLanes []string
	if in.Kind != "START" {
		issue := DotsIssue{Identifier: "hk-" + strconv.FormatInt(task.ID, 10), Title: task.Title, URL: fmt.Sprintf("hk:task/%d", task.ID), IssueUUID: fmt.Sprintf("hk:%d", task.ID)}
		comment := DotsComment{ID: commentID, CreatedAt: now.Format(time.RFC3339Nano), Body: "[dots] " + in.Kind + "\n" + in.Body}
		if _, ok := DotsKind(comment.Body); !ok {
			return DotsReplyResult{}, errors.New("invalid dots reply")
		}
		// The body document is editable after creation, so a bad notify line
		// cannot refuse the reply (that would roll back the RESULT). It is
		// skipped and reported in the result instead.
		var notifyLanes []string
		if in.Kind == "RESULT" {
			if notifyLanes, invalidLanes, err = dotsNotifyLanesTx(ctx, tx, task); err != nil {
				return DotsReplyResult{}, err
			}
		}
		docKey, _, err = recordDotsResultTx(ctx, tx, in.Kind, issue, comment, &task, task.ParentLane, "hk-mcp", notifyLanes...)
		if err != nil {
			return DotsReplyResult{}, err
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
		task, err = s.transitionTaskTx(ctx, tx, task.ID, TransitionInput{To: StateCompleted, By: actor, Evidence: "hk:doc " + docKey}, false)
	case "QUESTION":
		if in.AskOperator {
			var decision DecisionRequestResult
			decision, err = s.recordDecisionRequestTx(ctx, tx, task.ID, actor, DecisionRequestInput{Question: in.Body, Options: DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Provide an answer"}}, AllowFree: true}, DefaultAction: "Wait for an answer", Block: true})
			task = decision.Task
		}
	}
	if err != nil {
		return DotsReplyResult{}, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: task.ID, Kind: "source_event", By: actor, Note: payload + " " + task.State + " " + docKey, At: now, Source: "dots-hk-chatgpt", SourceRef: commentID, Field: "dots_reply", FromValue: in.Kind, ToValue: strconv.FormatInt(commentRowID, 10)}); err != nil {
		return DotsReplyResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DotsReplyResult{}, err
	}
	logYTCommentEnqueued(reply, replyOp, replyEnqueued)
	return DotsReplyResult{TaskID: task.ID, State: task.State, DocumentKey: docKey, RunToken: runToken, InvalidNotifyLanes: invalidLanes}, nil
}

// DotsAnswerInput carries the text answering a task's open dots question.
type DotsAnswerInput struct {
	Text string `json:"text"`
}

// DotsAnswerResult is the outcome of answering a task's open dots question.
type DotsAnswerResult struct {
	TaskID    int64              `json:"task_id"`
	RequestID string             `json:"request_id"`
	CommentID int64              `json:"comment_id"`
	Task      Task               `json:"task"`
	Request   DecisionRequest    `json:"request"`
	Comment   CreatedTaskComment `json:"comment"`
}

// AnswerDotsQuestion resolves a task's open dots decision request (kind answered,
// responder = caller) and records the answer text as a task comment, in one
// transaction. A requester-lane comment alone does not resolve it.
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DotsAnswerResult{}, err
	}
	defer tx.Rollback(ctx)

	// Lock order matches ReplyDots: task-mutation
	// advisory lock (level 2), then taskCommentInsertLock, then task row FOR UPDATE.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return DotsAnswerResult{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		return DotsAnswerResult{}, err
	}

	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DotsAnswerResult{}, ErrTaskNotFound
		}
		return DotsAnswerResult{}, err
	}

	if x.Lane != "dots" || !IsHKDotsRequest(x) {
		return DotsAnswerResult{}, errors.New("invalid dots request task")
	}
	if x.Refs.Disposition != nil {
		return DotsAnswerResult{}, ErrDispositionOperatorOnly
	}
	if isTerminalTaskState(x.State) {
		return DotsAnswerResult{}, ErrTaskTerminal
	}

	current := x.Refs.DecisionRequest
	if current == nil {
		return DotsAnswerResult{}, ErrNoOpenDecisionRequest
	}
	if current.Status != DecisionRequestOpen {
		return DotsAnswerResult{}, ErrDecisionRequestResolved
	}
	if current.EffectivePolicy(false) == DecisionPolicyOperatorDirect {
		return DotsAnswerResult{}, ErrDecisionRequestHumanOnly
	}

	now := time.Now().UTC()

	// 1. Insert comment
	var comment TaskComment
	err = scanTaskComment(tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING `+taskCommentColumns, taskID, in.Text, caller, now), &comment)
	if err != nil {
		return DotsAnswerResult{}, err
	}
	op, enqueued, err := s.enqueueYTCommentTx(ctx, tx, comment)
	if err != nil {
		return DotsAnswerResult{}, err
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
	attemptID, err := insertDecisionAttempt(ctx, tx, attempt)
	if err != nil {
		return DotsAnswerResult{}, err
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
		if others, err = otherBlockingDecisionsTx(ctx, tx, x.ID, resolved.ID); err != nil {
			return DotsAnswerResult{}, err
		}
		x.AwaitingDecision = others > 0
	}

	encoded, err := json.Marshal(x.Refs)
	if err != nil {
		return DotsAnswerResult{}, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET refs=$2::jsonb,awaiting_decision=$4,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, x.ID, string(encoded), now, x.AwaitingDecision), &x); err != nil {
		return DotsAnswerResult{}, err
	}

	var resolutionID int64
	err = tx.QueryRow(ctx, `INSERT INTO decision_resolutions(request_id,question_revision,revision_request_id,task_id,kind,option_id,text,receipt,provenance,channel,decided_by,recorded_by,attempt_id,committed_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,$12,$13,$14) RETURNING resolution_id`,
		root, current.Revision, resolved.ID, x.ID, DecisionRequestAnswered, "", in.Text, "", DecisionProvenanceOperatorRelay, DecisionChannelCLI,
		caller, caller, attemptID, now).Scan(&resolutionID)
	if err != nil {
		return DotsAnswerResult{}, err
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
	if err = s.queueDecisionAnswerNotice(ctx, tx, x, resolved, resolveIn, caller, resolutionID, now); err != nil {
		return DotsAnswerResult{}, err
	}
	if flagWas && !x.AwaitingDecision {
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: TaskEventAwaiting, From: x.State, To: x.State, By: caller, Note: "decision-clear " + resolved.ID + " by " + caller, Refs: x.Refs, Into: &x, At: now, Field: "awaiting_decision", FromValue: "true", ToValue: "false"}); err != nil {
			return DotsAnswerResult{}, err
		}
	}
	if err = insertDecisionEvent(ctx, tx, &x, caller, note, now); err != nil {
		return DotsAnswerResult{}, err
	}
	if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, x, caller, note, 0, now); err != nil {
		return DotsAnswerResult{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return DotsAnswerResult{}, err
	}
	logYTCommentEnqueued(comment, op, enqueued)
	projection := ytProjectionOf(op, enqueued)

	return DotsAnswerResult{
		TaskID:    taskID,
		RequestID: resolved.ID,
		CommentID: comment.ID,
		Task:      x,
		Request:   resolved,
		Comment:   CreatedTaskComment{TaskComment: comment, YouTrackProjection: &projection},
	}, nil
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

// dotsLaneText makes a notification drainable: the hub rejects C0/C1 control
// characters and text over the lane-event byte cap.
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
	if task.BodyDoc == "" {
		return nil, nil, nil
	}
	key, _, _ := strings.Cut(task.BodyDoc, "#")
	var body string
	if err := tx.QueryRow(ctx, `SELECT body FROM documents WHERE key=$1`, key).Scan(&body); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
func recordDotsResultTx(ctx context.Context, tx pgx.Tx, kind string, issue DotsIssue, c DotsComment, task *Task, lane, createdBy string, notifyLanes ...string) (key string, created bool, err error) {
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
	tag, err := tx.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,origin_class,task_id) VALUES($1,'note',$2,'',$3,$4,$5,$6,$6,'import',$7) ON CONFLICT(key) DO NOTHING`, key, DotsLane, body, hex.EncodeToString(sum[:]), createdBy, now, taskID)
	if err != nil {
		return "", false, err
	}
	created = tag.RowsAffected() == 1
	if createdBy == "hk-mcp" && !created {
		return "", false, errors.New("dots reply document key collision")
	}
	if !created && taskID != nil {
		if _, err := tx.Exec(ctx, `UPDATE documents SET task_id=$1 WHERE key=$2 AND task_id IS NULL`, *taskID, key); err != nil {
			return "", false, err
		}
	}
	if !progress {
		if _, err := tx.Exec(ctx, `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,verdict,artefact_sha256,task_id,routed_lane,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		ON CONFLICT(comment_id) DO UPDATE SET task_id=EXCLUDED.task_id, updated_at=EXCLUDED.updated_at
		WHERE dots_results.task_id IS NULL AND EXCLUDED.task_id IS NOT NULL`,
			key, c.ID, issue.IssueUUID, issue.Identifier, kind, verdict, hex.EncodeToString(artefact[:]), taskID, lane, now); err != nil {
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

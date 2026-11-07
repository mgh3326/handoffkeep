package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// An operator command is an action the console cannot perform itself: it is
// recorded in hk, delivered as one relay lane.event to the executor that owns
// the target, and completed only by that executor's receipt. The ladder never
// shows done before the receipt (design/2026-11-07/console-operator-control-design §2).

const (
	OperatorCommandKindWorkerMessage = "worker.message"
	OperatorCommandKindJobStop       = "job.stop"
	OperatorCommandKindTaskRespawn   = "task.respawn"
	OperatorCommandKindDeployApprove = "deploy.approve"
)

const (
	OperatorCommandStateRecorded   = "recorded"
	OperatorCommandStateDelivered  = "delivered"
	OperatorCommandStateAcked      = "acked"
	OperatorCommandStateDone       = "done"
	OperatorCommandStateFailed     = "failed"
	OperatorCommandStateExpired    = "expired"
	OperatorCommandStateSuperseded = "superseded"
)

const (
	OperatorCommandIntentSteer = "steer"
	OperatorCommandIntentQueue = "queue"

	// OperatorCommandWorkerMessageExpiry is worker.message's default deadline:
	// an instruction nobody picked up in half an hour is stale.
	OperatorCommandWorkerMessageExpiry = 30 * time.Minute

	operatorCommandKeyMaxBytes   = 128
	operatorCommandTextMaxBytes  = 1000
	operatorCommandNoteMaxBytes  = 1024
	operatorCommandErrorMaxBytes = 100
)

var (
	// ErrOperatorCommandInvalid refuses a command or receipt the store cannot
	// record as asked.
	ErrOperatorCommandInvalid = errors.New("invalid_operator_command")
	// ErrOperatorCommandNotFound is a receipt for an unknown command id.
	ErrOperatorCommandNotFound = errors.New("operator_command_not_found")
	// ErrOperatorCommandState refuses a backward or repeated-conflicting
	// transition; the reason names the current state.
	ErrOperatorCommandState = errors.New("operator_command_state")
	// ErrOperatorCommandExpired refuses a receipt after the deadline.
	ErrOperatorCommandExpired = errors.New("operator_command_expired")
	// ErrOperatorCommandNotExecutable is a reserved kind whose executor does
	// not exist yet.
	ErrOperatorCommandNotExecutable = errors.New("operator_command_not_executable")
	// ErrOperatorCommandNoExecutor refuses worker.message on a task with no
	// live job to address.
	ErrOperatorCommandNoExecutor = errors.New("operator_command_no_executor")
)

var operatorCommandStates = map[string]bool{
	OperatorCommandStateRecorded: true, OperatorCommandStateDelivered: true, OperatorCommandStateAcked: true,
	OperatorCommandStateDone: true, OperatorCommandStateFailed: true, OperatorCommandStateExpired: true, OperatorCommandStateSuperseded: true,
}

var operatorCommandKinds = map[string]bool{
	OperatorCommandKindWorkerMessage: true, OperatorCommandKindJobStop: true,
	OperatorCommandKindTaskRespawn: true, OperatorCommandKindDeployApprove: true,
}

var operatorCommandOpenStates = map[string]bool{
	OperatorCommandStateRecorded: true, OperatorCommandStateDelivered: true, OperatorCommandStateAcked: true,
}

// OperatorCommand is one recorded operator intent.
type OperatorCommand struct {
	ID            int64           `json:"id"`
	CommandKey    string          `json:"command_key"`
	Kind          string          `json:"kind"`
	TargetType    string          `json:"target_type"`
	TargetID      string          `json:"target_id"`
	TaskID        *int64          `json:"task_id,omitempty"`
	ExecutorLane  string          `json:"executor_lane"`
	Payload       json.RawMessage `json:"payload"`
	Actor         string          `json:"actor"`
	Channel       string          `json:"channel"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	LastErrorCode string          `json:"last_error_code"`
	CreatedAt     time.Time       `json:"created_at"`
	DeliveredAt   *time.Time      `json:"delivered_at,omitempty"`
	AckedAt       *time.Time      `json:"acked_at,omitempty"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
	ExpiresAt     time.Time       `json:"expires_at"`
}

// WorkerMessagePayload is worker.message's validated payload.
type WorkerMessagePayload struct {
	Text   string `json:"text"`
	Intent string `json:"intent"`
}

// OperatorCommandInput is one operator's request to record a command. The
// store derives the target and executor lane for the kind it knows.
type OperatorCommandInput struct {
	CommandKey string          `json:"command_key"`
	Kind       string          `json:"kind"`
	TaskID     *int64          `json:"task_id,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	Actor      string          `json:"actor"`
	Channel    string          `json:"channel"`
}

// OperatorCommandReceiptInput is an executor's report about a command. By is
// the authenticated API client, set by the handler and never read from the
// request body.
type OperatorCommandReceiptInput struct {
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
	Note      string `json:"note,omitempty"`
	By        string `json:"-"`
}

const operatorCommandColumns = `id,command_key,kind,target_type,target_id,task_id,executor_lane,payload,actor,channel,state,attempts,last_error_code,created_at,delivered_at,acked_at,finished_at,expires_at`

func scanOperatorCommand(row interface{ Scan(...any) error }, x *OperatorCommand) error {
	return row.Scan(&x.ID, &x.CommandKey, &x.Kind, &x.TargetType, &x.TargetID, &x.TaskID, &x.ExecutorLane, &x.Payload,
		&x.Actor, &x.Channel, &x.State, &x.Attempts, &x.LastErrorCode, &x.CreatedAt, &x.DeliveredAt, &x.AckedAt, &x.FinishedAt, &x.ExpiresAt)
}

// checkOperatorCommandKey explains why a command key is unusable. The key is
// the idempotency key: it must be a value the caller can reproduce exactly.
func checkOperatorCommandKey(key string) error {
	if strings.TrimSpace(key) == "" || !validText(key, operatorCommandKeyMaxBytes) {
		return fmt.Errorf("%w: command_key must be 1..%d bytes", ErrOperatorCommandInvalid, operatorCommandKeyMaxBytes)
	}
	return nil
}

// decodeWorkerMessagePayload decodes the one executable kind's payload with
// unknown fields refused, then validates it.
func decodeWorkerMessagePayload(raw []byte) (WorkerMessagePayload, error) {
	var p WorkerMessagePayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return p, fmt.Errorf("%w: payload must be {text, intent}", ErrOperatorCommandInvalid)
	}
	p.Text = strings.TrimSpace(p.Text)
	if len(p.Text) == 0 || len(p.Text) > operatorCommandTextMaxBytes || !validText(p.Text, operatorCommandTextMaxBytes) {
		return p, fmt.Errorf("%w: text must be 1..%d bytes", ErrOperatorCommandInvalid, operatorCommandTextMaxBytes)
	}
	if err := guard.Reject(p.Text); err != nil {
		return p, err
	}
	if p.Intent != OperatorCommandIntentSteer && p.Intent != OperatorCommandIntentQueue {
		return p, fmt.Errorf("%w: intent must be steer or queue", ErrOperatorCommandInvalid)
	}
	return p, nil
}

// operatorCommandLaneText is the relay lane.event body the executor receives.
// The marker is the documented contract: a queue-capable executor holds the
// instruction until the worker's current turn ends; one that cannot queue
// treats it as steer and says so in its receipt.
func operatorCommandLaneText(id int64, p WorkerMessagePayload) string {
	intent := "지금 끼어들기"
	if p.Intent == OperatorCommandIntentQueue {
		intent = "현재 턴 뒤"
	}
	return assistantLaneText(fmt.Sprintf("[hk 명령 #%d · %s] ", id, intent), laneSafeText(p.Text))
}

// laneSafeText removes the control characters validateLaneEventText refuses,
// turning line breaks into spaces so the text survives the relay unchanged in
// meaning.
func laneSafeText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		}
		return r
	}, s)
}

func operatorCommandKindLabel(kind string) string {
	switch kind {
	case OperatorCommandKindWorkerMessage:
		return "워커에게 지시"
	case OperatorCommandKindJobStop:
		return "워커 중지"
	case OperatorCommandKindTaskRespawn:
		return "작업 재시작"
	case OperatorCommandKindDeployApprove:
		return "배포 승인"
	}
	return kind
}

// operatorCommandSummary is the short instruction echoed in the task timeline.
func operatorCommandSummary(p WorkerMessagePayload) string {
	text := p.Text
	if len(text) > 120 {
		cut := 120
		for cut > 0 && text[cut]&0xc0 == 0x80 {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// operatorCommandStateNote is the Korean task_events note for one transition.
func operatorCommandStateNote(c OperatorCommand, p WorkerMessagePayload, state, errorCode, receiptNote string) string {
	label := operatorCommandKindLabel(c.Kind)
	switch state {
	case OperatorCommandStateRecorded:
		intent := "지금 끼어들기"
		if p.Intent == OperatorCommandIntentQueue {
			intent = "현재 턴 뒤"
		}
		return fmt.Sprintf("명령 기록 · %s(%s): %s", label, intent, operatorCommandSummary(p))
	case OperatorCommandStateDelivered:
		return "명령 전달됨 · " + label
	case OperatorCommandStateAcked:
		return appendReceiptNote("명령 접수됨 · "+label, receiptNote)
	case OperatorCommandStateDone:
		return appendReceiptNote("명령 완료 · "+label, receiptNote)
	case OperatorCommandStateFailed:
		note := "명령 실패 · " + label
		if errorCode != "" {
			note += " · 오류 코드: " + errorCode
		}
		return appendReceiptNote(note, receiptNote)
	case OperatorCommandStateExpired:
		return "명령 만료 · " + label + " · 기한 내 접수 없음"
	case OperatorCommandStateSuperseded:
		return "새 명령으로 대체됨 · " + label
	}
	return label
}

func appendReceiptNote(note, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return note
	}
	if len(extra) > 200 {
		cut := 200
		for cut > 0 && extra[cut]&0xc0 == 0x80 {
			cut--
		}
		extra = extra[:cut] + "…"
	}
	return note + ": " + extra
}

// operatorCommandTask carries the task snapshot a command event is written
// against.
type operatorCommandTask struct {
	State string
	Refs  TaskRefs
}

func loadOperatorCommandTask(ctx context.Context, q rowQuerier, id int64) (operatorCommandTask, error) {
	var out operatorCommandTask
	var refs []byte
	if err := q.QueryRow(ctx, `SELECT state,refs FROM tasks WHERE id=$1`, id).Scan(&out.State, &refs); err != nil {
		return out, err
	}
	if len(refs) > 0 {
		if err := json.Unmarshal(refs, &out.Refs); err != nil {
			return out, err
		}
	}
	return out, nil
}

// operatorCommandEventInput is one transition of a command into the task
// timeline. By names the writer: the operator, the executor that reported, or
// system for delivery/expiry/supersede.
type operatorCommandEventInput struct {
	Command   OperatorCommand
	Payload   WorkerMessagePayload
	State     string
	ErrorCode string
	Note      string
	// Prefix marks a receipt that arrived after the ladder closed (superseded
	// or expired); the command keeps its terminal state.
	Prefix string
	By     string
	// Auto keeps a system transition out of the task's activity clock: an
	// expiry nobody acted on must not make the task look freshly touched.
	Auto bool
	At   time.Time
}

// appendOperatorCommandEvent writes one command transition into the task
// timeline. Commands without a task write no event; the source_ref makes a
// replayed transition a no-op.
func appendOperatorCommandEvent(ctx context.Context, tx pgx.Tx, in operatorCommandEventInput) error {
	if in.Command.TaskID == nil {
		return nil
	}
	task, err := loadOperatorCommandTask(ctx, tx, *in.Command.TaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	by := in.By
	if by == "" {
		by = in.Command.Actor
	}
	_, _, err = insertEvent(ctx, tx, taskEventInput{
		TaskID: *in.Command.TaskID, Kind: "metadata", From: task.State, To: task.State, By: by,
		Note: in.Prefix + operatorCommandStateNote(in.Command, in.Payload, in.State, in.ErrorCode, in.Note), Refs: task.Refs, At: in.At,
		Source: "operator-command", SourceRef: in.Command.CommandKey + ":" + in.State, Auto: in.Auto,
	})
	return err
}

// findOperatorCommandByKey is the idempotency read. It runs before the task
// and executor checks so a retry of an already-recorded command returns the
// original row even after the worker's job ended or the task moved on.
func (s *Store) findOperatorCommandByKey(ctx context.Context, key string) (OperatorCommand, bool, error) {
	var x OperatorCommand
	err := scanOperatorCommand(s.pool.QueryRow(ctx, `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE command_key=$1`, key), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, false, nil
	}
	return x, err == nil, err
}

// cancelOperatorCommandRelayTx closes a relay event that will never be
// executed: the command was superseded or expired before delivery. The relay
// poller only picks up delivered_at IS NULL rows, and delivered_to names why
// it was closed, so the worker never receives a stale instruction.
func cancelOperatorCommandRelayTx(ctx context.Context, tx pgx.Tx, lane, eventID, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE relay_events SET delivered_at=$4, delivered_to=$3
		WHERE kind='lane.event' AND owner_lane=$1 AND event_id=$2 AND delivered_at IS NULL`, lane, eventID, "cancelled:"+reason, at)
	return err
}

// RecordOperatorCommand records one operator command and, in the same
// transaction, relays it to the executor lane. A repeated command_key returns
// the original row and writes nothing.
func (s *Store) RecordOperatorCommand(ctx context.Context, in OperatorCommandInput) (OperatorCommand, error) {
	var out OperatorCommand
	if err := checkOperatorCommandKey(in.CommandKey); err != nil {
		return out, err
	}
	if !operatorCommandKinds[in.Kind] {
		return out, fmt.Errorf("%w: unknown kind %q", ErrOperatorCommandInvalid, in.Kind)
	}
	if in.Kind != OperatorCommandKindWorkerMessage {
		return out, fmt.Errorf("%w: %s has no executor yet", ErrOperatorCommandNotExecutable, in.Kind)
	}
	if in.TaskID == nil || *in.TaskID < 1 {
		return out, fmt.Errorf("%w: worker.message requires a task", ErrOperatorCommandInvalid)
	}
	payload, err := decodeWorkerMessagePayload(in.Payload)
	if err != nil {
		return out, err
	}
	if !validText(in.Actor, 128) || strings.TrimSpace(in.Actor) == "" || !validText(in.Channel, 64) || strings.TrimSpace(in.Channel) == "" {
		return out, fmt.Errorf("%w: actor and channel are required", ErrOperatorCommandInvalid)
	}
	if existing, found, err := s.findOperatorCommandByKey(ctx, in.CommandKey); err != nil {
		return out, err
	} else if found {
		return existing, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// The record locks a task row's event stream and supersedes older rows:
	// the task-mutation lock comes first (lock order, lockorder.go).
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return out, err
	}
	if _, err = loadOperatorCommandTask(ctx, tx, *in.TaskID); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrTaskNotFound
	}
	if err != nil {
		return out, err
	}
	var lane string
	err = tx.QueryRow(ctx, `SELECT owner_lane FROM jobs WHERE task_id=$1 AND state IN ('claimed','spawned') ORDER BY created_at DESC, job_id DESC LIMIT 1`, *in.TaskID).Scan(&lane)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && strings.TrimSpace(lane) == "") {
		return out, ErrOperatorCommandNoExecutor
	}
	if err != nil {
		return out, err
	}
	now := time.Now().UTC()
	err = scanOperatorCommand(tx.QueryRow(ctx, `INSERT INTO operator_commands(command_key,kind,target_type,target_id,task_id,executor_lane,payload,actor,channel,state,attempts,last_error_code,created_at,expires_at)
		VALUES($1,$2,'task',$3,$4,$5,$6::jsonb,$7,$8,'recorded',0,'',$9,$10)
		ON CONFLICT (command_key) DO NOTHING RETURNING `+operatorCommandColumns,
		in.CommandKey, in.Kind, strconv.FormatInt(*in.TaskID, 10), *in.TaskID, lane, string(encoded), in.Actor, in.Channel, now, now.Add(OperatorCommandWorkerMessageExpiry)), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		// The key was recorded before: a replay returns the original command.
		if err = scanOperatorCommand(tx.QueryRow(ctx, `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE command_key=$1`, in.CommandKey), &out); err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	if err != nil {
		return out, err
	}
	// A newer command of the same kind and target supersedes the open ones.
	rows, err := tx.Query(ctx, `UPDATE operator_commands SET state='superseded', finished_at=$4
		WHERE kind=$1 AND target_type=$2 AND target_id=$3 AND state IN ('recorded','delivered','acked') AND id<>$5
		RETURNING `+operatorCommandColumns, out.Kind, out.TargetType, out.TargetID, now, out.ID)
	if err != nil {
		return out, err
	}
	var superseded []OperatorCommand
	for rows.Next() {
		var old OperatorCommand
		if err = scanOperatorCommand(rows, &old); err != nil {
			rows.Close()
			return out, err
		}
		superseded = append(superseded, old)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	for _, old := range superseded {
		if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{Command: old, Payload: payload, State: OperatorCommandStateSuperseded, By: "system", Auto: true, At: now}); err != nil {
			return out, err
		}
		// The replacement supersedes an instruction the worker has not seen:
		// close its relay event so the drainer cannot deliver it afterwards.
		if err = cancelOperatorCommandRelayTx(ctx, tx, old.ExecutorLane, old.CommandKey, OperatorCommandStateSuperseded, now); err != nil {
			return out, err
		}
	}
	if _, _, err = s.appendRelayEventTx(ctx, tx, RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: in.CommandKey, Text: operatorCommandLaneText(out.ID, payload), EventTime: &now}); err != nil {
		return out, err
	}
	if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{Command: out, Payload: payload, State: OperatorCommandStateRecorded, At: now}); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// RecordOperatorCommandReceipt applies an executor's ack/done/failed report.
// Transitions move forward only and a repeated receipt is a no-op; a failure
// is legal from recorded, and a receipt for a superseded/expired command is
// kept as a late report in the timeline without reopening the ladder.
func (s *Store) RecordOperatorCommandReceipt(ctx context.Context, id int64, in OperatorCommandReceiptInput) (OperatorCommand, error) {
	var out OperatorCommand
	if id < 1 {
		return out, ErrOperatorCommandNotFound
	}
	switch in.State {
	case OperatorCommandStateAcked, OperatorCommandStateDone, OperatorCommandStateFailed:
	default:
		return out, fmt.Errorf("%w: receipt state must be acked, done or failed", ErrOperatorCommandInvalid)
	}
	if !validText(in.ErrorCode, operatorCommandErrorMaxBytes) || !validText(in.Note, operatorCommandNoteMaxBytes) {
		return out, fmt.Errorf("%w: receipt text is too long", ErrOperatorCommandInvalid)
	}
	if err := guard.Reject(in.ErrorCode); err != nil {
		return out, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if err := guard.Reject(in.Note); err != nil {
		return out, err
	}
	by := strings.TrimSpace(in.By)
	if by == "" {
		by = "executor"
	}
	if !validText(by, 128) {
		return out, fmt.Errorf("%w: receipt caller is too long", ErrOperatorCommandInvalid)
	}
	if in.State == OperatorCommandStateFailed && strings.TrimSpace(in.ErrorCode) == "" {
		return out, fmt.Errorf("%w: failed needs an error_code", ErrOperatorCommandInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = scanOperatorCommand(tx.QueryRow(ctx, `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE id=$1 FOR UPDATE`, id), &out); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrOperatorCommandNotFound
		}
		return out, err
	}
	now := time.Now().UTC()
	if out.State == in.State {
		return out, tx.Commit(ctx)
	}
	if !operatorCommandOpenStates[out.State] {
		// A superseded or expired command can still have run: the executor had
		// already received it when the ladder closed. Record the late report in
		// the timeline and keep the terminal state — never lose the fact that it
		// executed, and never pretend it completed as the operator's ladder.
		if out.State == OperatorCommandStateSuperseded || out.State == OperatorCommandStateExpired {
			prefix := "대체 후 보고 · "
			if out.State == OperatorCommandStateExpired {
				prefix = "만료 후 보고 · "
			}
			if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{
				Command: out, State: in.State, ErrorCode: in.ErrorCode, Note: in.Note,
				Prefix: prefix, By: "executor:" + by, At: now,
			}); err != nil {
				return out, err
			}
			return out, tx.Commit(ctx)
		}
		return out, fmt.Errorf("%w: %s cannot become %s", ErrOperatorCommandState, out.State, in.State)
	}
	if now.After(out.ExpiresAt) {
		if _, err = tx.Exec(ctx, `UPDATE operator_commands SET state='expired', finished_at=$2 WHERE id=$1`, id, now); err != nil {
			return out, err
		}
		out.State, out.FinishedAt = OperatorCommandStateExpired, &now
		if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{Command: out, State: OperatorCommandStateExpired, By: "system", Auto: true, At: now}); err != nil {
			return out, err
		}
		if err = cancelOperatorCommandRelayTx(ctx, tx, out.ExecutorLane, out.CommandKey, OperatorCommandStateExpired, now); err != nil {
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		return out, ErrOperatorCommandExpired
	}
	allowed := (in.State == OperatorCommandStateAcked && (out.State == OperatorCommandStateRecorded || out.State == OperatorCommandStateDelivered)) ||
		(in.State == OperatorCommandStateDone && (out.State == OperatorCommandStateDelivered || out.State == OperatorCommandStateAcked)) ||
		// An executor that cannot deliver reports the failure before the relay
		// is confirmed; recorded is a legal start for a failed receipt.
		(in.State == OperatorCommandStateFailed && operatorCommandOpenStates[out.State])
	if !allowed {
		return out, fmt.Errorf("%w: %s cannot become %s", ErrOperatorCommandState, out.State, in.State)
	}
	var deliveredAt, ackedAt, finishedAt *time.Time
	if out.DeliveredAt == nil && in.State == OperatorCommandStateAcked {
		deliveredAt = &now
	}
	if in.State == OperatorCommandStateAcked {
		ackedAt = &now
	} else {
		finishedAt = &now
	}
	if _, err = tx.Exec(ctx, `UPDATE operator_commands SET state=$2, delivered_at=COALESCE(delivered_at,$3), acked_at=COALESCE(acked_at,$4), finished_at=$5, last_error_code=$6 WHERE id=$1`,
		id, in.State, deliveredAt, ackedAt, finishedAt, in.ErrorCode); err != nil {
		return out, err
	}
	out.State, out.DeliveredAt, out.AckedAt, out.FinishedAt, out.LastErrorCode = in.State, firstTime(out.DeliveredAt, deliveredAt), firstTime(out.AckedAt, ackedAt), finishedAt, in.ErrorCode
	if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{
		Command: out, State: in.State, ErrorCode: in.ErrorCode, Note: in.Note, By: "executor:" + by, At: now,
	}); err != nil {
		return out, err
	}
	// A failure can arrive before delivery (executor_unavailable,
	// queue_unsupported); close the undelivered relay so it cannot reach the
	// worker after the executor already gave up.
	if in.State == OperatorCommandStateFailed || in.State == OperatorCommandStateDone {
		if err = cancelOperatorCommandRelayTx(ctx, tx, out.ExecutorLane, out.CommandKey, in.State, now); err != nil {
			return out, err
		}
	}
	return out, tx.Commit(ctx)
}

func firstTime(current, next *time.Time) *time.Time {
	if current != nil {
		return current
	}
	return next
}

// markOperatorCommandsDeliveredTx moves commands to delivered when the relay
// event that carries them is confirmed delivered to the executor pane. The
// state guard makes a replayed delivery a no-op.
func markOperatorCommandsDeliveredTx(ctx context.Context, tx pgx.Tx, lane, eventID string, at time.Time) error {
	rows, err := tx.Query(ctx, `UPDATE operator_commands SET state='delivered', delivered_at=$3
		WHERE executor_lane=$1 AND command_key=$2 AND state='recorded'
		RETURNING `+operatorCommandColumns, lane, eventID, at)
	if err != nil {
		return err
	}
	var delivered []OperatorCommand
	for rows.Next() {
		var c OperatorCommand
		if err = scanOperatorCommand(rows, &c); err != nil {
			rows.Close()
			return err
		}
		delivered = append(delivered, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, c := range delivered {
		if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{Command: c, State: OperatorCommandStateDelivered, By: "system", Auto: true, At: at}); err != nil {
			return err
		}
	}
	return nil
}

// ExpireOperatorCommands moves every open command past its deadline to
// expired and records each in its task timeline. It is called before the
// console and executor lists read, so a stale command never renders as open.
func (s *Store) ExpireOperatorCommands(ctx context.Context) (int, error) {
	// Polls call this before every read. The no-op path takes no lock and no
	// transaction, so an executor polling a lane never queues behind the
	// task-mutation lock for nothing.
	var due bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_commands WHERE state IN ('recorded','delivered','acked') AND expires_at < now())`).Scan(&due); err != nil || !due {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `UPDATE operator_commands SET state='expired', finished_at=$1
		WHERE state IN ('recorded','delivered','acked') AND expires_at < $1
		RETURNING `+operatorCommandColumns, now)
	if err != nil {
		return 0, err
	}
	var expired []OperatorCommand
	for rows.Next() {
		var c OperatorCommand
		if err = scanOperatorCommand(rows, &c); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, c := range expired {
		if err = appendOperatorCommandEvent(ctx, tx, operatorCommandEventInput{Command: c, State: OperatorCommandStateExpired, By: "system", Auto: true, At: now}); err != nil {
			return 0, err
		}
		// An undelivered instruction past its deadline must not reach the worker.
		if err = cancelOperatorCommandRelayTx(ctx, tx, c.ExecutorLane, c.CommandKey, OperatorCommandStateExpired, now); err != nil {
			return 0, err
		}
	}
	return len(expired), tx.Commit(ctx)
}

// ListOperatorCommands pages commands for one executor lane and state in id
// order. The state filter is optional; afterID is exclusive.
func (s *Store) ListOperatorCommands(ctx context.Context, executorLane, state string, afterID int64, limit int) ([]OperatorCommand, error) {
	if !validText(executorLane, 128) || strings.TrimSpace(executorLane) == "" || (state != "" && !operatorCommandStates[state]) || afterID < 0 {
		return nil, fmt.Errorf("%w: invalid list query", ErrOperatorCommandInvalid)
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if _, err := s.ExpireOperatorCommands(ctx); err != nil {
		return nil, err
	}
	q, args := `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE executor_lane=$1 AND id>$2`, []any{executorLane, afterID}
	if state != "" {
		args = append(args, state)
		q += fmt.Sprintf(" AND state=$%d", len(args))
	}
	q += " ORDER BY id ASC LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OperatorCommand{}
	for rows.Next() {
		var c OperatorCommand
		if err = scanOperatorCommand(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListOperatorCommandsByTask returns a task's commands newest first. The
// limit is inclusive and the caller reports truncation.
func (s *Store) ListOperatorCommandsByTask(ctx context.Context, taskID int64, limit int) ([]OperatorCommand, error) {
	if taskID < 1 {
		return nil, fmt.Errorf("%w: invalid task", ErrOperatorCommandInvalid)
	}
	if limit < 1 {
		limit = 100
	}
	if _, err := s.ExpireOperatorCommands(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE task_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OperatorCommand{}
	for rows.Next() {
		var c OperatorCommand
		if err = scanOperatorCommand(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OperatorCommandOpen is one in-flight command as the console row chip shows
// it: the target task, kind and current state.
type OperatorCommandOpen struct {
	TaskID    int64     `json:"task_id"`
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// ReadOpenOperatorCommands returns at most one open command per task, newest
// first, for the console's row chip.
func (s *Store) ReadOpenOperatorCommands(ctx context.Context, taskIDs []int64) (map[int64]OperatorCommandOpen, error) {
	out := make(map[int64]OperatorCommandOpen, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	if _, err := s.ExpireOperatorCommands(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (task_id) task_id,id,kind,state,created_at
		FROM operator_commands WHERE task_id = ANY($1) AND state IN ('recorded','delivered','acked')
		ORDER BY task_id, created_at DESC, id DESC`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x OperatorCommandOpen
		if err := rows.Scan(&x.TaskID, &x.ID, &x.Kind, &x.State, &x.CreatedAt); err != nil {
			return nil, err
		}
		out[x.TaskID] = x
	}
	return out, rows.Err()
}

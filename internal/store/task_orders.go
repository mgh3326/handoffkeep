package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A task order is the operator's "set going" for one task that has not started
// (hk 5125, docs/task-orders.md). It records the operator's words, the channel
// and the time, and queues one notification to the lane that owns the task's
// project. It starts no worker and carries no approval: the lane reads the
// order from this record, acknowledges it, and settles, briefs and spawns as
// before.
const (
	// The channels an order comes through. console and cli are verified (a
	// Cloudflare Access operator, the operator bearer); dots relays the
	// operator's words and is never verified.
	TaskOrderChannelConsole = "console"
	TaskOrderChannelDots    = "dots"
	TaskOrderChannelCLI     = "cli"

	// MaxTaskOrderWordsBytes bounds the operator's words. The lane notice
	// carries them next to the order id, title and channel inside the hub's
	// text limit; the full words stay here.
	MaxTaskOrderWordsBytes = 2000

	taskOrderSource         = "task-order"
	taskOrderAckSource      = "task-order-ack"
	taskOrderWithdrawSource = "task-order-withdraw"
	taskOrderEventField     = "order"
)

var (
	// ErrOrderTaskStarted refuses an order for a task past backlog, unstarted
	// or triage. The text after the code names the state.
	ErrOrderTaskStarted = errors.New("order_task_started")
	// ErrOrderNoLane refuses an order whose task has neither a project design
	// lane nor a lane of its own to deliver it to.
	ErrOrderNoLane = errors.New("order_no_lane")
	// ErrOrderKeyConflict refuses an idempotency key that already recorded an
	// order with other words or for another task.
	ErrOrderKeyConflict = errors.New("order_key_conflict")
	// ErrOrderNotOpen refuses an ack of a withdrawn order, or a withdraw of an
	// acknowledged one: the lane wait clears once.
	ErrOrderNotOpen = errors.New("order_not_open")
	// ErrOrderNotFound is an ack or withdraw of an order that does not exist.
	ErrOrderNotFound = errors.New("order_not_found")
	// ErrOrderInvalid is a malformed order write: words, key, actor, channel,
	// id or reason the store refuses before it opens a transaction.
	ErrOrderInvalid = errors.New("order_invalid")
)

var taskOrderIDRE = regexp.MustCompile(`^ord-[a-z2-7]{10}$`)

// ValidTaskOrderID reports whether id has the shape of an order id.
func ValidTaskOrderID(id string) bool { return taskOrderIDRE.MatchString(id) }

// TaskOrder is one order. Open is true until it is acknowledged or withdrawn;
// an open order waits on its target lane (WaitsOnLane), and nothing else
// holds that wait. DeliveredAt is the outbox row's hub receipt, read from
// there rather than copied.
type TaskOrder struct {
	ID             string     `json:"id"`
	TaskID         int64      `json:"task_id"`
	Words          string     `json:"words"`
	Channel        string     `json:"channel"`
	Actor          string     `json:"actor"`
	Verified       bool       `json:"verified"`
	CreatedAt      time.Time  `json:"created_at"`
	TargetLane     string     `json:"target_lane"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	AckedAt        *time.Time `json:"acked_at"`
	AckedBy        *string    `json:"acked_by"`
	AckCommentID   *int64     `json:"ack_comment_id"`
	WithdrawnAt    *time.Time `json:"withdrawn_at"`
	WithdrawnBy    *string    `json:"withdrawn_by"`
	WithdrawReason *string    `json:"withdraw_reason"`
	Open           bool       `json:"open"`
	WaitsOnLane    string     `json:"waits_on_lane"`
}

// TaskOrderResult is an order write's answer. Duplicate is true when the write
// found the order already in the state it asked for and changed nothing.
type TaskOrderResult struct {
	Order     TaskOrder `json:"order"`
	Duplicate bool      `json:"duplicate"`
}

// CreateTaskOrderInput is one order write. Channel and Verified come from the
// caller's authenticated identity, never from the request body: the store
// refuses a dots order marked verified and a console or cli order that is not.
type CreateTaskOrderInput struct {
	TaskID   int64
	Words    string
	Actor    string
	Channel  string
	Verified bool
	Key      string
}

const taskOrderBareColumns = `id,task_id,words,channel,actor,verified,created_at,target_lane,acked_at,acked_by,ack_comment_id,withdrawn_at,withdrawn_by,withdraw_reason`

// taskOrderViewSQL reads an order with its delivery receipt. The outbox row is
// the single record of delivery, so the order carries no copy of it.
const taskOrderViewSQL = `SELECT o.id,o.task_id,o.words,o.channel,o.actor,o.verified,o.created_at,o.target_lane,n.sent_at,o.acked_at,o.acked_by,o.ack_comment_id,o.withdrawn_at,o.withdrawn_by,o.withdraw_reason
FROM task_orders o LEFT JOIN notification_outbox n ON n.event_id = CONCAT('task-order-', o.id)`

var placeholderRE = regexp.MustCompile(`\$[0-9]+`)

// orderSQL answers a PostgreSQL statement whose $n parameters appear once each
// and in order, as the same statement for the store's dialect.
func (s *Store) orderSQL(postgres string) string {
	return s.q(postgres, placeholderRE.ReplaceAllString(postgres, "?"))
}

func scanTaskOrderView(r row, x *TaskOrder) error {
	if err := r.Scan(&x.ID, &x.TaskID, &x.Words, &x.Channel, &x.Actor, &x.Verified, &x.CreatedAt, &x.TargetLane, &x.DeliveredAt,
		&x.AckedAt, &x.AckedBy, &x.AckCommentID, &x.WithdrawnAt, &x.WithdrawnBy, &x.WithdrawReason); err != nil {
		return err
	}
	x.Open = x.AckedAt == nil && x.WithdrawnAt == nil
	x.WaitsOnLane = ""
	if x.Open {
		x.WaitsOnLane = x.TargetLane
	}
	return nil
}

func scanTaskOrderBare(r row, x *TaskOrder) error {
	if err := r.Scan(&x.ID, &x.TaskID, &x.Words, &x.Channel, &x.Actor, &x.Verified, &x.CreatedAt, &x.TargetLane,
		&x.AckedAt, &x.AckedBy, &x.AckCommentID, &x.WithdrawnAt, &x.WithdrawnBy, &x.WithdrawReason); err != nil {
		return err
	}
	x.Open = x.AckedAt == nil && x.WithdrawnAt == nil
	x.WaitsOnLane = ""
	if x.Open {
		x.WaitsOnLane = x.TargetLane
	}
	return nil
}

func newTaskOrderID() (string, error) {
	var b [7]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ord-" + strings.ToLower(base32.StdEncoding.EncodeToString(b[:]))[:10], nil
}

// orderableState reports whether a task in state has not started: the only
// states an order may name.
func orderableState(state string) bool {
	switch state {
	case StateBacklog, StateUnstarted, StateTriage:
		return true
	}
	return false
}

func taskOrderNoticeEventID(orderID string) string { return "task-order-" + orderID }

func taskOrderWithdrawnNoticeEventID(orderID string) string {
	return "task-order-withdrawn-" + orderID
}

func (c CreateTaskOrderInput) check() error {
	switch {
	case c.TaskID < 1:
		return fmt.Errorf("%w: task id", ErrOrderInvalid)
	case strings.TrimSpace(c.Words) == "" || !validText(c.Words, MaxTaskOrderWordsBytes):
		return fmt.Errorf("%w: words must be 1..%d bytes", ErrOrderInvalid, MaxTaskOrderWordsBytes)
	case !ValidActor(c.Actor):
		return fmt.Errorf("%w: actor", ErrOrderInvalid)
	case c.Key == "" || !validText(c.Key, maxIdempotencyKeyBytes):
		return fmt.Errorf("%w: idempotency_key is required, at most %d bytes", ErrOrderInvalid, maxIdempotencyKeyBytes)
	}
	switch c.Channel {
	case TaskOrderChannelConsole, TaskOrderChannelCLI:
		if !c.Verified {
			return fmt.Errorf("%w: a console or cli order is verified", ErrOrderInvalid)
		}
	case TaskOrderChannelDots:
		if c.Verified {
			return fmt.Errorf("%w: a dots order relays the operator's words and is not verified", ErrOrderInvalid)
		}
	default:
		return fmt.Errorf("%w: channel", ErrOrderInvalid)
	}
	return guard.Reject(c.Words)
}

// CreateTaskOrder records an order and queues its lane notice, in one
// transaction under the task-mutation lock and the task row lock (lockorder.go,
// levels 2 and 4; the order, its event and the outbox row are level 5). An order
// changes nothing on the task but its activity clock: no state, lane, refs or
// decision request.
//
// A key this actor already used replays the first order when the task and the
// words are the same, ahead of every other check, so a retry survives the task
// starting meanwhile; other words or another task are ErrOrderKeyConflict.
// Otherwise the task must not have started (ErrOrderTaskStarted) and needs a
// lane to deliver to: the project's design lane, else its own (ErrOrderNoLane).
func (s *Store) CreateTaskOrder(ctx context.Context, in CreateTaskOrderInput) (TaskOrderResult, error) {
	if err := in.check(); err != nil {
		return TaskOrderResult{}, err
	}
	var out TaskOrderResult
	err := s.retryTx(ctx, func() error {
		out = TaskOrderResult{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
			return err
		}
		var x Task
		if err = scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), in.TaskID), &x); err != nil {
			if errors.Is(err, ErrNoRows) {
				return ErrTaskNotFound
			}
			return err
		}
		var prior TaskOrder
		err = scanTaskOrderView(tx.QueryRow(ctx, s.orderSQL(taskOrderViewSQL+` WHERE o.actor=$1 AND o.idempotency_key=$2`), in.Actor, in.Key), &prior)
		switch {
		case err == nil:
			if prior.TaskID != in.TaskID || prior.Words != in.Words {
				return ErrOrderKeyConflict
			}
			out = TaskOrderResult{Order: prior, Duplicate: true}
			return tx.Commit(ctx)
		case !errors.Is(err, ErrNoRows):
			return err
		}
		if !orderableState(x.State) {
			return fmt.Errorf("%w: task %d is %s; only backlog, unstarted or triage can be ordered", ErrOrderTaskStarted, x.ID, x.State)
		}
		target := ""
		if x.Project != nil {
			err = tx.QueryRow(ctx, s.q(`SELECT design_lane FROM task_projects WHERE name=$1`, `SELECT design_lane FROM task_projects WHERE name=?`), *x.Project).Scan(&target)
			if err != nil && !errors.Is(err, ErrNoRows) {
				return err
			}
		}
		if target == "" {
			target = x.Lane
		}
		if target == "" {
			return ErrOrderNoLane
		}
		id, err := newTaskOrderID()
		if err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err = tx.Exec(ctx, s.orderSQL(`INSERT INTO task_orders(id,task_id,words,channel,actor,verified,idempotency_key,created_at,target_lane) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`),
			id, in.TaskID, in.Words, in.Channel, in.Actor, in.Verified, in.Key, now, target); err != nil {
			return err
		}
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: x.State, To: x.State, By: in.Actor,
			Note: "order " + id + " via " + in.Channel + ": " + in.Words, Refs: x.Refs, Into: &x, At: now,
			Source: taskOrderSource, SourceRef: id, Field: taskOrderEventField, FromValue: "none", ToValue: "open"}); err != nil {
			return err
		}
		if err = s.queueTaskOrderNotice(ctx, tx, x, id, target, in, now); err != nil {
			return err
		}
		if err = scanTaskOrderView(tx.QueryRow(ctx, s.orderSQL(taskOrderViewSQL+` WHERE o.id=$1`), id), &out.Order); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return TaskOrderResult{}, err
	}
	return out, nil
}

// queueTaskOrderNotice writes the lane notice for a new order: the order id,
// the task and its title, the operator's words and the channel. It says in so
// many words that the order approves nothing, and points at the ack command.
func (s *Store) queueTaskOrderNotice(ctx context.Context, tx transaction, x Task, orderID, lane string, in CreateTaskOrderInput, at time.Time) error {
	trust := "verified"
	if !in.Verified {
		trust = "relayed"
	}
	taskID := strconv.FormatInt(x.ID, 10)
	body := fmt.Sprintf("%s | 운영자 지시: %q | 채널: %s (%s) | 승인(머지, 배포, 비용, 시크릿, 정책)은 포함하지 않음 | 확인: handoffkeep tasks order-ack %s --comment <text>",
		x.Title, in.Words, in.Channel, trust, orderID)
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	return insertNotificationOutbox(ctx, tx, OutboxKindTaskOrder, lane, taskOrderNoticeEventID(orderID),
		assistantLaneText("[hk] 진행 지시 "+orderID+" task "+taskID+": ", body), at)
}

// lockTaskOrderTx takes the locks an order's ack or withdraw needs, in the
// order of lockorder.go: the task-mutation lock, then the task row, then the
// order row. It returns the task and the order as stored.
func (s *Store) lockTaskOrderTx(ctx context.Context, tx transaction, orderID string, commentLock bool) (Task, TaskOrder, error) {
	var taskID int64
	if err := tx.QueryRow(ctx, s.orderSQL(`SELECT task_id FROM task_orders WHERE id=$1`), orderID).Scan(&taskID); err != nil {
		if errors.Is(err, ErrNoRows) {
			return Task{}, TaskOrder{}, ErrOrderNotFound
		}
		return Task{}, TaskOrder{}, err
	}
	if err := s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
		return Task{}, TaskOrder{}, err
	}
	if commentLock {
		if err := s.lockTx(ctx, tx, taskCommentInsertLock); err != nil {
			return Task{}, TaskOrder{}, err
		}
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), taskID), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return Task{}, TaskOrder{}, ErrTaskNotFound
		}
		return Task{}, TaskOrder{}, err
	}
	var o TaskOrder
	if err := scanTaskOrderBare(tx.QueryRow(ctx, s.orderSQL(`SELECT `+taskOrderBareColumns+` FROM task_orders WHERE id=$1 FOR UPDATE`), orderID), &o); err != nil {
		if errors.Is(err, ErrNoRows) {
			return Task{}, TaskOrder{}, ErrOrderNotFound
		}
		return Task{}, TaskOrder{}, err
	}
	return x, o, nil
}

// AckTaskOrder records that the target lane has read the order: acked_at, who
// acknowledged it and a task comment carrying the lane's comment. It clears the
// order's lane wait and changes nothing else. An order already acknowledged
// answers Duplicate and writes nothing; a withdrawn one is ErrOrderNotOpen.
func (s *Store) AckTaskOrder(ctx context.Context, orderID, actor, comment string) (TaskOrderResult, error) {
	if !ValidTaskOrderID(orderID) || !ValidActor(actor) || strings.TrimSpace(comment) == "" {
		return TaskOrderResult{}, fmt.Errorf("%w: ack needs an order id, an actor and a comment", ErrOrderInvalid)
	}
	if err := CheckTaskCommentBody(comment); err != nil {
		return TaskOrderResult{}, err
	}
	var out TaskOrderResult
	err := s.retryTx(ctx, func() error {
		out = TaskOrderResult{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		x, o, err := s.lockTaskOrderTx(ctx, tx, orderID, true)
		if err != nil {
			return err
		}
		if o.AckedAt == nil && o.WithdrawnAt != nil {
			return ErrOrderNotOpen
		}
		if o.AckedAt == nil {
			now := time.Now().UTC().Truncate(time.Microsecond)
			body := "[hk] 진행 지시 " + orderID + " 확인 by " + actor + ": " + comment
			if err = s.srcEvSystemCommentTx(ctx, tx, x.ID, taskOrderAckSource, orderID, body, now); err != nil {
				return err
			}
			var commentID int64
			if err = tx.QueryRow(ctx, s.orderSQL(`SELECT id FROM task_comments WHERE task_id=$1 AND system_event_key=$2`), x.ID, systemTaskCommentKey(x.ID, taskOrderAckSource, orderID)).Scan(&commentID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, s.orderSQL(`UPDATE task_orders SET acked_at=$2,acked_by=$3,ack_comment_id=$4 WHERE id=$1`), orderID, now, actor, commentID); err != nil {
				return err
			}
			if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: x.State, To: x.State, By: actor,
				Note: "order " + orderID + " acknowledged: " + comment, Refs: x.Refs, Into: &x, At: now,
				Source: taskOrderAckSource, SourceRef: orderID, Field: taskOrderEventField, FromValue: "open", ToValue: "acked"}); err != nil {
				return err
			}
		} else {
			out.Duplicate = true
		}
		if err = scanTaskOrderView(tx.QueryRow(ctx, s.orderSQL(taskOrderViewSQL+` WHERE o.id=$1`), orderID), &out.Order); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return TaskOrderResult{}, err
	}
	return out, nil
}

// WithdrawTaskOrder withdraws an open order: withdrawn_at, who withdrew it and
// why, a task event, and its own notice to the same lane. It clears the lane
// wait. An order already withdrawn answers Duplicate and writes nothing; an
// acknowledged one is ErrOrderNotOpen. The caller has checked that the actor is
// an operator principal.
func (s *Store) WithdrawTaskOrder(ctx context.Context, orderID, actor, reason string) (TaskOrderResult, error) {
	reason = strings.TrimSpace(reason)
	if !ValidTaskOrderID(orderID) || !ValidActor(actor) || reason == "" || !validText(reason, DecisionRequestReasonMaxBytes) {
		return TaskOrderResult{}, fmt.Errorf("%w: withdraw needs an order id, an actor and a reason", ErrOrderInvalid)
	}
	if err := guard.Reject(reason); err != nil {
		return TaskOrderResult{}, err
	}
	var out TaskOrderResult
	err := s.retryTx(ctx, func() error {
		out = TaskOrderResult{}
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		x, o, err := s.lockTaskOrderTx(ctx, tx, orderID, false)
		if err != nil {
			return err
		}
		if o.AckedAt != nil {
			return ErrOrderNotOpen
		}
		if o.WithdrawnAt == nil {
			now := time.Now().UTC().Truncate(time.Microsecond)
			if _, err = tx.Exec(ctx, s.orderSQL(`UPDATE task_orders SET withdrawn_at=$2,withdrawn_by=$3,withdraw_reason=$4 WHERE id=$1`), orderID, now, actor, reason); err != nil {
				return err
			}
			if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: x.State, To: x.State, By: actor,
				Note: "order " + orderID + " withdrawn: " + reason, Refs: x.Refs, Into: &x, At: now,
				Source: taskOrderWithdrawSource, SourceRef: orderID, Field: taskOrderEventField, FromValue: "open", ToValue: "withdrawn"}); err != nil {
				return err
			}
			taskID := strconv.FormatInt(x.ID, 10)
			body := fmt.Sprintf("%s | 사유: %s | by %s | 이 지시로는 시작하지 말 것", x.Title, reason, actor)
			if base := s.decisionConsoleURL(); base != "" {
				body += " | " + base + "/ui/tasks/" + taskID
			}
			if err = insertNotificationOutbox(ctx, tx, OutboxKindTaskOrder, o.TargetLane, taskOrderWithdrawnNoticeEventID(orderID), assistantLaneText("[hk] 진행 지시 철회 "+orderID+" task "+taskID+": ", body), now); err != nil {
				return err
			}
		} else {
			out.Duplicate = true
		}
		if err = scanTaskOrderView(tx.QueryRow(ctx, s.orderSQL(taskOrderViewSQL+` WHERE o.id=$1`), orderID), &out.Order); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return TaskOrderResult{}, err
	}
	return out, nil
}

// GetTaskOrder reads one order.
func (s *Store) GetTaskOrder(ctx context.Context, id string) (TaskOrder, bool, error) {
	var x TaskOrder
	if err := scanTaskOrderView(s.db().QueryRow(ctx, s.orderSQL(taskOrderViewSQL+` WHERE o.id=$1`), id), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return TaskOrder{}, false, nil
		}
		return TaskOrder{}, false, err
	}
	return x, true, nil
}

// TaskOrderListMax bounds one task's order list.
const TaskOrderListMax = 100

// ListTaskOrders lists a task's orders, oldest first. openOnly keeps the ones
// still waiting on their lane. A missing task is ErrTaskNotFound.
func (s *Store) ListTaskOrders(ctx context.Context, taskID int64, openOnly bool) ([]TaskOrder, error) {
	if taskID < 1 {
		return nil, fmt.Errorf("%w: task id", ErrOrderInvalid)
	}
	var exists bool
	if err := s.db().QueryRow(ctx, s.orderSQL(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`), taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrTaskNotFound
	}
	where := ` WHERE o.task_id=$1`
	if openOnly {
		where += ` AND o.acked_at IS NULL AND o.withdrawn_at IS NULL`
	}
	rs, err := s.db().Query(ctx, s.orderSQL(taskOrderViewSQL+where+` ORDER BY o.created_at,o.id LIMIT `+strconv.Itoa(TaskOrderListMax)), taskID)
	if err != nil {
		return nil, err
	}
	return collectRows(rs, func(r row) (TaskOrder, error) {
		var x TaskOrder
		return x, scanTaskOrderView(r, &x)
	})
}

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Dialect-neutral cases for task_orders.go (registry: tasks_area_cases_test.go).
// Each case maps to a row of the invariant table in docs/task-orders.md.

func init() {
	registerTaskAreaCase("orders/create_records_order_event_and_notice", ordersCaseCreate)
	registerTaskAreaCase("orders/touches_nothing_on_the_task", ordersCaseTouchesNothing)
	registerTaskAreaCase("orders/channel_and_input_checks", ordersCaseInputChecks)
	registerTaskAreaCase("orders/idempotency", ordersCaseIdempotency)
	registerTaskAreaCase("orders/only_not_started_tasks", ordersCaseNotStarted)
	registerTaskAreaCase("orders/target_lane", ordersCaseTargetLane)
	registerTaskAreaCase("orders/ack_clears_the_lane_wait_once", ordersCaseAck)
	registerTaskAreaCase("orders/withdraw_clears_the_lane_wait_once", ordersCaseWithdraw)
	registerTaskAreaCase("orders/close_states_are_exclusive", ordersCaseExclusiveClose)
	registerTaskAreaCase("orders/list_and_get", ordersCaseListGet)
}

type ordersFixture struct {
	task       Task
	project    string
	designLane string
}

// ordersTask files a backlog task in a fresh project owned by a design lane.
func ordersTask(t *testing.T, s *Store, title string) ordersFixture {
	t.Helper()
	ctx := t.Context()
	n := srcEvUnique()
	project := fmt.Sprintf("orders-%d", n)
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	design := fmt.Sprintf("orders-design-%d", n)
	if _, err := s.SetProjectDesignLane(ctx, project, design, "tester", "orders case"); err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: fmt.Sprintf("orders-lane-%d", n), Title: title, Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return ordersFixture{task: x, project: project, designLane: design}
}

func ordersInput(f ordersFixture, key string) CreateTaskOrderInput {
	return CreateTaskOrderInput{TaskID: f.task.ID, Words: "이 일 진행시켜", Actor: "operator", Channel: TaskOrderChannelCLI, Verified: true, Key: key}
}

type ordersCounts struct{ orders, events, notices int }

// ordersCount counts the rows an order writes for one task: orders, order task
// events and task_order outbox rows addressed through the orders' ids.
func ordersCount(t *testing.T, s *Store, taskID int64) ordersCounts {
	t.Helper()
	ctx := t.Context()
	var c ordersCounts
	list, err := s.ListTaskOrders(ctx, taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	c.orders = len(list)
	x := srcEvGet(t, s, taskID)
	for _, e := range x.Events {
		if e.Field == taskOrderEventField {
			c.events++
		}
	}
	ids := map[string]bool{}
	for _, o := range list {
		ids[taskOrderNoticeEventID(o.ID)] = true
		ids[taskOrderWithdrawnNoticeEventID(o.ID)] = true
	}
	for _, n := range ordersOutbox(t, s) {
		if ids[n.EventID] {
			c.notices++
		}
	}
	return c
}

func ordersOutbox(t *testing.T, s *Store) []NotificationOutbox {
	t.Helper()
	rows, err := s.ListUnsentNotifications(t.Context(), 200, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []NotificationOutbox
	for _, n := range rows {
		if n.Kind == OutboxKindTaskOrder {
			out = append(out, n)
		}
	}
	return out
}

func ordersCaseCreate(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "order me")
	got, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-create"))
	if err != nil || got.Duplicate {
		t.Fatalf("create: %+v err=%v", got, err)
	}
	o := got.Order
	if !ValidTaskOrderID(o.ID) || o.TaskID != f.task.ID || o.Words != "이 일 진행시켜" || o.Channel != TaskOrderChannelCLI || o.Actor != "operator" ||
		!o.Verified || o.TargetLane != f.designLane || !o.Open || o.WaitsOnLane != f.designLane || o.AckedAt != nil || o.WithdrawnAt != nil || o.DeliveredAt != nil {
		t.Fatalf("order: %+v", o)
	}
	// One order, one task event and one outbox row, to the design lane.
	if c := ordersCount(t, s, f.task.ID); c != (ordersCounts{1, 1, 1}) {
		t.Fatalf("rows after create = %+v, want one each", c)
	}
	notices := ordersOutbox(t, s)
	if len(notices) != 1 || notices[0].EventID != "task-order-"+o.ID || notices[0].TargetLane != f.designLane {
		t.Fatalf("outbox: %+v", notices)
	}
	for _, want := range []string{o.ID, fmt.Sprintf("task %d", f.task.ID), "order me", "이 일 진행시켜", "cli", "verified"} {
		if !strings.Contains(notices[0].Text, want) {
			t.Errorf("notice text lacks %q: %s", want, notices[0].Text)
		}
	}
	x := srcEvGet(t, s, f.task.ID)
	var ev *TaskEvent
	for i := range x.Events {
		if x.Events[i].Field == taskOrderEventField {
			ev = &x.Events[i]
		}
	}
	if ev == nil || ev.Kind != "metadata" || ev.Source != taskOrderSource || ev.SourceRef != o.ID || ev.By != "operator" || ev.ToValue != "open" {
		t.Fatalf("order event: %+v", ev)
	}
}

func ordersCaseTouchesNothing(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "widens nothing")
	before := srcEvGet(t, s, f.task.ID)
	if _, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-nothing")); err != nil {
		t.Fatal(err)
	}
	after := srcEvGet(t, s, f.task.ID)
	if after.State != before.State || after.Lane != before.Lane || after.Priority != before.Priority || after.Title != before.Title ||
		after.AwaitingDecision != before.AwaitingDecision || after.ClaimedBy != before.ClaimedBy || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("an order changed the task: before=%+v after=%+v", before, after)
	}
	if after.Refs.DecisionRequest != before.Refs.DecisionRequest || after.Refs.Closure != nil {
		t.Fatalf("an order changed the task's refs: before=%+v after=%+v", before.Refs, after.Refs)
	}
}

func ordersCaseInputChecks(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "input checks")
	base := ordersInput(f, "k-input")
	for name, mutate := range map[string]func(*CreateTaskOrderInput){
		"no key":              func(in *CreateTaskOrderInput) { in.Key = "" },
		"no words":            func(in *CreateTaskOrderInput) { in.Words = "  " },
		"overlong words":      func(in *CreateTaskOrderInput) { in.Words = strings.Repeat("a", MaxTaskOrderWordsBytes+1) },
		"no actor":            func(in *CreateTaskOrderInput) { in.Actor = "" },
		"unknown channel":     func(in *CreateTaskOrderInput) { in.Channel = "mcp" },
		"cli not verified":    func(in *CreateTaskOrderInput) { in.Verified = false },
		"console not verfied": func(in *CreateTaskOrderInput) { in.Channel, in.Verified = TaskOrderChannelConsole, false },
		"dots verified":       func(in *CreateTaskOrderInput) { in.Channel, in.Verified = TaskOrderChannelDots, true },
		"bad task id":         func(in *CreateTaskOrderInput) { in.TaskID = 0 },
	} {
		in := base
		mutate(&in)
		if _, err := s.CreateTaskOrder(ctx, in); err == nil || errors.Is(err, ErrTaskNotFound) {
			t.Errorf("%s: err=%v, want a validation error", name, err)
		}
	}
	if _, err := s.CreateTaskOrder(ctx, CreateTaskOrderInput{TaskID: 1 << 40, Words: "go", Actor: "operator", Channel: TaskOrderChannelCLI, Verified: true, Key: "k-missing"}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	if c := ordersCount(t, s, f.task.ID); c != (ordersCounts{}) {
		t.Fatalf("refused orders left rows: %+v", c)
	}
	// A dots order relays the operator's words and is recorded as unverified.
	relay := base
	relay.Key, relay.Channel, relay.Verified, relay.Actor = "k-dots", TaskOrderChannelDots, false, "dots"
	got, err := s.CreateTaskOrder(ctx, relay)
	if err != nil || got.Order.Verified || got.Order.Channel != TaskOrderChannelDots {
		t.Fatalf("dots order: %+v err=%v", got, err)
	}
	if n := ordersOutbox(t, s); len(n) != 1 || !strings.Contains(n[0].Text, "relayed") {
		t.Fatalf("a relayed order's notice must say so: %+v", n)
	}
}

func ordersCaseIdempotency(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "replayed")
	first, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-same"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-same"))
	if err != nil || !again.Duplicate || again.Order.ID != first.Order.ID {
		t.Fatalf("replay: %+v err=%v, want duplicate of %s", again, err, first.Order.ID)
	}
	if c := ordersCount(t, s, f.task.ID); c != (ordersCounts{1, 1, 1}) {
		t.Fatalf("a replay wrote rows: %+v", c)
	}
	other := ordersInput(f, "k-same")
	other.Words = "다른 말"
	if _, err := s.CreateTaskOrder(ctx, other); !errors.Is(err, ErrOrderKeyConflict) {
		t.Fatalf("same key, other words: err=%v, want ErrOrderKeyConflict", err)
	}
	elsewhere := ordersTask(t, s, "another task")
	if _, err := s.CreateTaskOrder(ctx, ordersInput(elsewhere, "k-same")); !errors.Is(err, ErrOrderKeyConflict) {
		t.Fatalf("same key, other task: err=%v, want ErrOrderKeyConflict", err)
	}
	// The key belongs to the actor: another actor's order with it is its own.
	mine := ordersInput(f, "k-same")
	mine.Actor = "operator-two"
	if got, err := s.CreateTaskOrder(ctx, mine); err != nil || got.Duplicate || got.Order.ID == first.Order.ID {
		t.Fatalf("another actor's key: %+v err=%v", got, err)
	}
	// A replay is answered before the task-state check: it survives the task
	// having started in the meantime.
	if _, err := s.ClaimTask(ctx, f.task.ID, "worker", "", "orders case"); err != nil {
		t.Fatal(err)
	}
	late, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-same"))
	if err != nil || !late.Duplicate || late.Order.ID != first.Order.ID {
		t.Fatalf("replay after start: %+v err=%v", late, err)
	}
}

func ordersCaseNotStarted(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "already going")
	started, err := s.ClaimTask(ctx, f.task.ID, "worker", "", "orders case")
	if err != nil {
		t.Fatal(err)
	}
	if orderableState(started.State) {
		t.Fatalf("claimed task is %s, which an order may name", started.State)
	}
	_, err = s.CreateTaskOrder(ctx, ordersInput(f, "k-started"))
	if !errors.Is(err, ErrOrderTaskStarted) || !strings.Contains(err.Error(), started.State) {
		t.Fatalf("started task: err=%v, want ErrOrderTaskStarted naming %q", err, started.State)
	}
	if c := ordersCount(t, s, f.task.ID); c != (ordersCounts{}) || len(ordersOutbox(t, s)) != 0 {
		t.Fatalf("a refused order left rows: %+v", c)
	}
	for _, state := range []string{StateBacklog, StateUnstarted, StateTriage} {
		if !orderableState(state) {
			t.Errorf("%s must be orderable", state)
		}
	}
	for _, state := range []string{StateResearching, StateDeveloping, StateReviewing, StateMerged, StateDeployed, StateCompleted, StateDropped, StateDuplicate, StateIcebox} {
		if orderableState(state) {
			t.Errorf("%s must not be orderable", state)
		}
	}
}

func ordersCaseTargetLane(t *testing.T, s *Store) {
	ctx := t.Context()
	// A project with a design lane delivers to it.
	owned := ordersTask(t, s, "owned project")
	got, err := s.CreateTaskOrder(ctx, ordersInput(owned, "k-owned"))
	if err != nil || got.Order.TargetLane != owned.designLane {
		t.Fatalf("design lane: %+v err=%v", got, err)
	}
	// Without one, the task's own lane.
	bare := ordersTask(t, s, "bare project")
	if _, err := s.SetProjectDesignLane(ctx, bare.project, "", "tester", "clear"); err != nil {
		t.Fatal(err)
	}
	got, err = s.CreateTaskOrder(ctx, ordersInput(bare, "k-bare"))
	if err != nil || got.Order.TargetLane != bare.task.Lane || got.Order.WaitsOnLane != bare.task.Lane {
		t.Fatalf("task lane: %+v err=%v, want %s", got, err, bare.task.Lane)
	}
	// With neither, the order is refused and nothing is written.
	none := ordersTask(t, s, "no lane")
	if _, err := s.SetProjectDesignLane(ctx, none.project, "", "tester", "clear"); err != nil {
		t.Fatal(err)
	}
	// tasks writes go through begin: MySQL tags the sync rows its triggers write.
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, s.orderSQL(`UPDATE tasks SET lane='' WHERE id=$1`), none.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskOrder(ctx, ordersInput(none, "k-none")); !errors.Is(err, ErrOrderNoLane) {
		t.Fatalf("no lane: err=%v, want ErrOrderNoLane", err)
	}
	if c := ordersCount(t, s, none.task.ID); c != (ordersCounts{}) {
		t.Fatalf("a refused order left rows: %+v", c)
	}
}

func ordersCaseAck(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "acked")
	created, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-ack"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Order.ID
	acked, err := s.AckTaskOrder(ctx, id, "design-lane", "읽었음, 곧 시작")
	if err != nil || acked.Duplicate {
		t.Fatalf("ack: %+v err=%v", acked, err)
	}
	o := acked.Order
	if o.AckedAt == nil || o.AckedBy == nil || *o.AckedBy != "design-lane" || o.AckCommentID == nil || o.Open || o.WaitsOnLane != "" || o.WithdrawnAt != nil {
		t.Fatalf("acked order: %+v", o)
	}
	comments, err := s.ListTaskComments(ctx, f.task.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, c := range comments {
		if c.ID == *o.AckCommentID && strings.Contains(c.Body, id) && strings.Contains(c.Body, "읽었음, 곧 시작") && strings.Contains(c.Body, "design-lane") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("ack comment %d not found once in %+v", *o.AckCommentID, comments)
	}
	// The wait clears once: a second ack, even by another actor, changes nothing.
	again, err := s.AckTaskOrder(ctx, id, "someone-else", "또 확인")
	if err != nil || !again.Duplicate || again.Order.AckedBy == nil || *again.Order.AckedBy != "design-lane" || !again.Order.AckedAt.Equal(*o.AckedAt) {
		t.Fatalf("second ack: %+v err=%v", again, err)
	}
	after, err := s.ListTaskComments(ctx, f.task.ID, 0, 50)
	if err != nil || len(after) != len(comments) {
		t.Fatalf("second ack wrote a comment: %d -> %d err=%v", len(comments), len(after), err)
	}
	if open, err := s.ListTaskOrders(ctx, f.task.ID, true); err != nil || len(open) != 0 {
		t.Fatalf("open orders after ack: %+v err=%v", open, err)
	}
	// Ack changes no task state.
	if x := srcEvGet(t, s, f.task.ID); x.State != f.task.State || x.Lane != f.task.Lane {
		t.Fatalf("ack changed the task: %+v", x)
	}
	for name, call := range map[string]func() error{
		"unknown order": func() error { _, err := s.AckTaskOrder(ctx, "ord-aaaaaaaaaa", "x", "c"); return err },
	} {
		if err := call(); !errors.Is(err, ErrOrderNotFound) {
			t.Errorf("%s: err=%v, want ErrOrderNotFound", name, err)
		}
	}
	if _, err := s.AckTaskOrder(ctx, id, "x", "  "); err == nil {
		t.Error("an ack without a comment was accepted")
	}
	if _, err := s.AckTaskOrder(ctx, "not-an-order", "x", "c"); err == nil {
		t.Error("an ack of a malformed id was accepted")
	}
}

func ordersCaseWithdraw(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "withdrawn")
	created, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-withdraw"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Order.ID
	got, err := s.WithdrawTaskOrder(ctx, id, "operator", "마음이 바뀜")
	if err != nil || got.Duplicate {
		t.Fatalf("withdraw: %+v err=%v", got, err)
	}
	o := got.Order
	if o.WithdrawnAt == nil || o.WithdrawnBy == nil || *o.WithdrawnBy != "operator" || o.WithdrawReason == nil || *o.WithdrawReason != "마음이 바뀜" || o.Open || o.WaitsOnLane != "" || o.AckedAt != nil {
		t.Fatalf("withdrawn order: %+v", o)
	}
	// Its own notice goes to the same lane, and the order's notice is untouched.
	var withdrawn *NotificationOutbox
	notices := ordersOutbox(t, s)
	for i := range notices {
		if notices[i].EventID == "task-order-withdrawn-"+id {
			withdrawn = &notices[i]
		}
	}
	if len(notices) != 2 || withdrawn == nil || withdrawn.TargetLane != f.designLane || !strings.Contains(withdrawn.Text, id) || !strings.Contains(withdrawn.Text, "마음이 바뀜") {
		t.Fatalf("notices after withdraw: %+v", notices)
	}
	// The second withdraw replays: no second notice, no second event.
	again, err := s.WithdrawTaskOrder(ctx, id, "operator", "또 철회")
	if err != nil || !again.Duplicate || *again.Order.WithdrawReason != "마음이 바뀜" {
		t.Fatalf("second withdraw: %+v err=%v", again, err)
	}
	if c := ordersCount(t, s, f.task.ID); c != (ordersCounts{1, 2, 2}) {
		t.Fatalf("rows after two withdraws = %+v, want 1 order, 2 events, 2 notices", c)
	}
	// A withdrawn order cannot be acknowledged.
	if _, err := s.AckTaskOrder(ctx, id, "design-lane", "late"); !errors.Is(err, ErrOrderNotOpen) {
		t.Fatalf("ack of a withdrawn order: err=%v, want ErrOrderNotOpen", err)
	}
	if _, err := s.WithdrawTaskOrder(ctx, id, "operator", " "); err == nil {
		t.Fatal("a withdraw without a reason was accepted")
	}
	if _, err := s.WithdrawTaskOrder(ctx, "ord-aaaaaaaaaa", "operator", "x"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("unknown order: err=%v, want ErrOrderNotFound", err)
	}
}

func ordersCaseExclusiveClose(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "acked then withdrawn")
	created, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-excl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AckTaskOrder(ctx, created.Order.ID, "design-lane", "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithdrawTaskOrder(ctx, created.Order.ID, "operator", "too late"); !errors.Is(err, ErrOrderNotOpen) {
		t.Fatalf("withdraw of an acked order: err=%v, want ErrOrderNotOpen", err)
	}
	// The database refuses both closes on one row, whatever the writer does.
	if _, err := s.db().Exec(ctx, s.orderSQL(`UPDATE task_orders SET withdrawn_at=$2,withdrawn_by=$3 WHERE id=$1`), created.Order.ID, time.Now().UTC(), "operator"); err == nil {
		t.Fatal("the database accepted an order that is both acked and withdrawn")
	}
	got, ok, err := s.GetTaskOrder(ctx, created.Order.ID)
	if err != nil || !ok || got.WithdrawnAt != nil || got.AckedAt == nil {
		t.Fatalf("order after a refused double close: %+v ok=%t err=%v", got, ok, err)
	}
}

func ordersCaseListGet(t *testing.T, s *Store) {
	ctx := t.Context()
	f := ordersTask(t, s, "listed")
	a, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-list-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTaskOrder(ctx, ordersInput(f, "k-list-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithdrawTaskOrder(ctx, a.Order.ID, "operator", "first one"); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListTaskOrders(ctx, f.task.ID, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("all orders: %+v err=%v", all, err)
	}
	open, err := s.ListTaskOrders(ctx, f.task.ID, true)
	if err != nil || len(open) != 1 || open[0].ID != b.Order.ID || open[0].WaitsOnLane != f.designLane {
		t.Fatalf("open orders: %+v err=%v", open, err)
	}
	if _, err := s.ListTaskOrders(ctx, 1<<40, false); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("list of a missing task: err=%v, want ErrTaskNotFound", err)
	}
	if _, ok, err := s.GetTaskOrder(ctx, "ord-aaaaaaaaaa"); err != nil || ok {
		t.Fatalf("get of a missing order: ok=%t err=%v", ok, err)
	}
	// Delivery is the outbox row's receipt, read through the order.
	if _, err := s.MarkNotificationSent(ctx, "task-order-"+b.Order.ID, 41); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetTaskOrder(ctx, b.Order.ID)
	if err != nil || !ok || got.DeliveredAt == nil {
		t.Fatalf("delivered order: %+v ok=%t err=%v", got, ok, err)
	}
}

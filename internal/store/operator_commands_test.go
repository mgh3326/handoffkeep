package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestOperatorCommandsMigrationRerunIsNoOp(t *testing.T) {
	_, url := testdb.Schema(t, "opcmd_mig")
	ctx := t.Context()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var appliedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE id='20261107-operator-commands'`).Scan(&appliedAt); err != nil {
		t.Fatalf("migration not recorded: %v", err)
	}
	var table bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass(format('%I.operator_commands', current_schema())) IS NOT NULL`).Scan(&table); err != nil || !table {
		t.Fatalf("operator_commands missing: %v", err)
	}
	var indexes int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND tablename='operator_commands' AND indexname IN ('operator_commands_task','operator_commands_executor')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("indexes=%v err=%v", indexes, err)
	}
	s.Close()

	again, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()
	var recordedAt time.Time
	var rows int
	if err := again.pool.QueryRow(ctx, `SELECT applied_at, (SELECT count(*) FROM schema_migrations WHERE id='20261107-operator-commands') FROM schema_migrations WHERE id='20261107-operator-commands'`).Scan(&recordedAt, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !recordedAt.Equal(appliedAt) {
		t.Fatalf("re-run changed the migration row: rows=%d at=%v want %v", rows, recordedAt, appliedAt)
	}
}

// operatorCommandFixture creates a task with a live job owned by lane.
func operatorCommandFixture(t *testing.T, st *Store, lane, jobID string) Task {
	t.Helper()
	task := linearTask(t, st, TaskRefs{})
	if _, _, err := st.ClaimJob(t.Context(), Job{JobID: jobID, TaskID: &task.ID, Host: "example-host", CreatedBy: "operator-command-test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `UPDATE jobs SET owner_lane=$2 WHERE job_id=$1`, jobID, lane); err != nil {
		t.Fatal(err)
	}
	return task
}

func recordWorkerMessage(t *testing.T, st *Store, taskID int64, key, text, intent string) OperatorCommand {
	t.Helper()
	payload, err := json.Marshal(WorkerMessagePayload{Text: text, Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	command, err := st.RecordOperatorCommand(t.Context(), OperatorCommandInput{
		CommandKey: key, Kind: OperatorCommandKindWorkerMessage, TaskID: &taskID,
		Payload: payload, Actor: "operator:test", Channel: "console",
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func commandByID(t *testing.T, st *Store, id int64) OperatorCommand {
	t.Helper()
	var c OperatorCommand
	if err := scanOperatorCommand(st.pool.QueryRow(t.Context(), `SELECT `+operatorCommandColumns+` FROM operator_commands WHERE id=$1`, id), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRecordOperatorCommandIsIdempotent(t *testing.T) {
	st := schemaStore(t, "opcmd_idem")
	lane, job := "opcmd-lane", "opcmd-job"
	task := operatorCommandFixture(t, st, lane, job)
	first := recordWorkerMessage(t, st, task.ID, "opcmd-key", "테스트부터 확인", OperatorCommandIntentSteer)
	second := recordWorkerMessage(t, st, task.ID, "opcmd-key", "다른 문장", OperatorCommandIntentQueue)
	if first.ID != second.ID || first.State != OperatorCommandStateRecorded || second.State != OperatorCommandStateRecorded {
		t.Fatalf("replay: first=%+v second=%+v", first, second)
	}
	if first.ExecutorLane != lane || first.TargetType != "task" || first.TargetID != fmt.Sprint(task.ID) {
		t.Fatalf("derived target: %+v", first)
	}
	var events, recorded, delivered int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, lane, "opcmd-key").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM task_events WHERE task_id=$1 AND source='operator-command'`, task.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM operator_commands WHERE task_id=$1`, task.ID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if events != 1 || recorded != 1 || delivered != 1 {
		t.Fatalf("replay wrote again: relay=%d events=%d commands=%d", events, recorded, delivered)
	}
	var text string
	if err := st.pool.QueryRow(t.Context(), `SELECT text FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, lane, "opcmd-key").Scan(&text); err != nil || !strings.Contains(text, "지금 끼어들기") || !strings.Contains(text, "테스트부터 확인") {
		t.Fatalf("relay text=%q err=%v", text, err)
	}
}

func TestOperatorCommandTransitionsMoveForwardOnly(t *testing.T) {
	st := schemaStore(t, "opcmd_flow")
	task := operatorCommandFixture(t, st, "opcmd-flow-lane", "opcmd-flow-job")
	command := recordWorkerMessage(t, st, task.ID, "opcmd-flow-key", "지시", OperatorCommandIntentSteer)
	ctx := t.Context()
	if _, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateDone}); !errors.Is(err, ErrOperatorCommandState) {
		t.Fatalf("done before delivery: %v", err)
	}
	acked, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateAcked})
	if err != nil || acked.State != OperatorCommandStateAcked || acked.AckedAt == nil || acked.DeliveredAt == nil {
		t.Fatalf("acked: %+v %v", acked, err)
	}
	replay, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateAcked})
	if err != nil || replay.State != OperatorCommandStateAcked {
		t.Fatalf("acked replay: %+v %v", replay, err)
	}
	done, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateDone, Note: "반영함"})
	if err != nil || done.State != OperatorCommandStateDone || done.FinishedAt == nil {
		t.Fatalf("done: %+v %v", done, err)
	}
	if _, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateFailed, ErrorCode: "late"}); !errors.Is(err, ErrOperatorCommandState) {
		t.Fatalf("failed after done: %v", err)
	}
	if _, err := st.RecordOperatorCommandReceipt(ctx, 999999, OperatorCommandReceiptInput{State: OperatorCommandStateAcked}); !errors.Is(err, ErrOperatorCommandNotFound) {
		t.Fatalf("missing command: %v", err)
	}
	var notes []string
	rows, err := st.pool.Query(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND source='operator-command' ORDER BY id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var note string
		if err = rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	if len(notes) != 3 || !strings.Contains(notes[0], "명령 기록") || !strings.Contains(notes[1], "명령 접수됨") || !strings.Contains(notes[2], "명령 완료") {
		t.Fatalf("task notes: %q", notes)
	}
}

func TestOperatorCommandExpiresAndSupersedes(t *testing.T) {
	st := schemaStore(t, "opcmd_expiry")
	task := operatorCommandFixture(t, st, "opcmd-expiry-lane", "opcmd-expiry-job")
	ctx := t.Context()
	first := recordWorkerMessage(t, st, task.ID, "opcmd-expiry-key", "첫 명령", OperatorCommandIntentSteer)
	second := recordWorkerMessage(t, st, task.ID, "opcmd-expiry-key-2", "두 번째 명령", OperatorCommandIntentQueue)
	old := commandByID(t, st, first.ID)
	if old.State != OperatorCommandStateSuperseded || old.FinishedAt == nil || commandByID(t, st, second.ID).State != OperatorCommandStateRecorded {
		t.Fatalf("supersede: first=%+v second=%+v", old, commandByID(t, st, second.ID))
	}
	// The superseded instruction must not reach the worker later: its relay
	// row is closed and the undelivered poll skips it.
	var deliveredTo string
	if err := st.pool.QueryRow(ctx, `SELECT delivered_to FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, "opcmd-expiry-lane", "opcmd-expiry-key").Scan(&deliveredTo); err != nil || deliveredTo != "cancelled:superseded" {
		t.Fatalf("superseded relay: %q %v", deliveredTo, err)
	}
	undelivered, err := st.ListRelayEvents(ctx, "opcmd-expiry-lane", true, 0)
	if err != nil || len(undelivered) != 1 || undelivered[0].EventID != "opcmd-expiry-key-2" {
		t.Fatalf("undelivered after supersede: %+v %v", undelivered, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE operator_commands SET expires_at=now()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := st.ExpireOperatorCommands(ctx)
	if err != nil || expired != 1 {
		t.Fatalf("expire: n=%d err=%v", expired, err)
	}
	if got := commandByID(t, st, second.ID); got.State != OperatorCommandStateExpired || got.FinishedAt == nil {
		t.Fatalf("expired row: %+v", got)
	}
	var expiredTo string
	if err := st.pool.QueryRow(ctx, `SELECT delivered_to FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, "opcmd-expiry-lane", "opcmd-expiry-key-2").Scan(&expiredTo); err != nil || expiredTo != "cancelled:expired" {
		t.Fatalf("expired relay: %q %v", expiredTo, err)
	}
	if n, err := st.ExpireOperatorCommands(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
	// An executor that had already received the expired command reports late;
	// the ladder keeps 만료 and the timeline records what happened.
	early, err := st.RecordOperatorCommandReceipt(ctx, second.ID, OperatorCommandReceiptInput{State: OperatorCommandStateAcked, By: "node-a"})
	if err != nil || early.State != OperatorCommandStateExpired {
		t.Fatalf("late ack: %+v %v", early, err)
	}
	late, err := st.RecordOperatorCommandReceipt(ctx, second.ID, OperatorCommandReceiptInput{State: OperatorCommandStateDone, By: "node-a"})
	if err != nil || late.State != OperatorCommandStateExpired {
		t.Fatalf("late report: %+v %v", late, err)
	}
	var lateNote, lateBy string
	if err := st.pool.QueryRow(ctx, `SELECT note, "by" FROM task_events WHERE task_id=$1 AND source='operator-command' AND source_ref=$2`, task.ID, "opcmd-expiry-key-2:done").Scan(&lateNote, &lateBy); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lateNote, "만료 후 보고 · ") || lateBy != "executor:node-a" {
		t.Fatalf("late event: note=%q by=%q", lateNote, lateBy)
	}
	var superseded, expiredNotes int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE note LIKE '새 명령으로 대체됨%'), count(*) FILTER (WHERE note LIKE '명령 만료%') FROM task_events WHERE task_id=$1 AND source='operator-command'`, task.ID).Scan(&superseded, &expiredNotes); err != nil {
		t.Fatal(err)
	}
	if superseded != 1 || expiredNotes != 1 {
		t.Fatalf("notes superseded=%d expired=%d", superseded, expiredNotes)
	}
}

func TestOperatorCommandDeliveryMovesItForward(t *testing.T) {
	st := schemaStore(t, "opcmd_delivery")
	lane := "opcmd-delivery-lane"
	task := operatorCommandFixture(t, st, lane, "opcmd-delivery-job")
	ctx := t.Context()
	command := recordWorkerMessage(t, st, task.ID, "opcmd-delivery-key", "지시", OperatorCommandIntentSteer)
	events, err := st.ListRelayEvents(ctx, lane, false, 0)
	if err != nil || len(events) != 1 || events[0].EventID != "opcmd-delivery-key" {
		t.Fatalf("relay events: %+v %v", events, err)
	}
	if _, err = st.MarkRelayEventDelivered(ctx, events[0].ID, "example-host", "pane-1"); err != nil {
		t.Fatal(err)
	}
	delivered := commandByID(t, st, command.ID)
	if delivered.State != OperatorCommandStateDelivered || delivered.DeliveredAt == nil {
		t.Fatalf("delivered: %+v", delivered)
	}
	// A replayed delivery is a no-op, not a second task event.
	if _, err = st.MarkRelayEventDelivered(ctx, events[0].ID, "example-host", "pane-1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND source='operator-command' AND source_ref LIKE '%:delivered'`, task.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("delivered events=%d err=%v", n, err)
	}
}

func TestOperatorCommandRefusals(t *testing.T) {
	st := schemaStore(t, "opcmd_refuse")
	ctx := t.Context()
	task := linearTask(t, st, TaskRefs{})
	payload, _ := json.Marshal(WorkerMessagePayload{Text: "지시", Intent: OperatorCommandIntentSteer})
	if _, err := st.RecordOperatorCommand(ctx, OperatorCommandInput{CommandKey: "k1", Kind: OperatorCommandKindWorkerMessage, TaskID: &task.ID, Payload: payload, Actor: "operator:test", Channel: "console"}); !errors.Is(err, ErrOperatorCommandNoExecutor) {
		t.Fatalf("no executor: %v", err)
	}
	task = operatorCommandFixture(t, st, "opcmd-refuse-lane", "opcmd-refuse-job")
	if _, err := st.RecordOperatorCommand(ctx, OperatorCommandInput{CommandKey: "k2", Kind: OperatorCommandKindJobStop, TaskID: &task.ID, Payload: payload, Actor: "operator:test", Channel: "console"}); !errors.Is(err, ErrOperatorCommandNotExecutable) {
		t.Fatalf("reserved kind: %v", err)
	}
	if _, err := st.RecordOperatorCommand(ctx, OperatorCommandInput{CommandKey: "k3", Kind: OperatorCommandKindWorkerMessage, TaskID: &task.ID, Payload: json.RawMessage(`{"text":"x","intent":"now","extra":1}`), Actor: "operator:test", Channel: "console"}); !errors.Is(err, ErrOperatorCommandInvalid) {
		t.Fatalf("bad payload: %v", err)
	}
}

func TestOperatorCommandFailureBeforeDelivery(t *testing.T) {
	st := schemaStore(t, "opcmd_earlyfail")
	task := operatorCommandFixture(t, st, "opcmd-earlyfail-lane", "opcmd-earlyfail-job")
	ctx := t.Context()
	command := recordWorkerMessage(t, st, task.ID, "opcmd-earlyfail-key", "지시", OperatorCommandIntentSteer)
	failed, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{
		State: OperatorCommandStateFailed, ErrorCode: "executor_unavailable", By: "node-a",
	})
	if err != nil || failed.State != OperatorCommandStateFailed || failed.DeliveredAt != nil || failed.FinishedAt == nil {
		t.Fatalf("early failure: %+v %v", failed, err)
	}
	var deliveredTo string
	if err := st.pool.QueryRow(ctx, `SELECT delivered_to FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, "opcmd-earlyfail-lane", "opcmd-earlyfail-key").Scan(&deliveredTo); err != nil || deliveredTo != "cancelled:failed" {
		t.Fatalf("failed relay: %q %v", deliveredTo, err)
	}
	if undelivered, err := st.ListRelayEvents(ctx, "opcmd-earlyfail-lane", true, 0); err != nil || len(undelivered) != 0 {
		t.Fatalf("undelivered after failure: %+v %v", undelivered, err)
	}
	var note string
	var auto bool
	if err := st.pool.QueryRow(ctx, `SELECT note, auto FROM task_events WHERE task_id=$1 AND source_ref=$2`, task.ID, "opcmd-earlyfail-key:failed").Scan(&note, &auto); err != nil || !strings.Contains(note, "오류 코드: executor_unavailable") || auto {
		t.Fatalf("failure event: note=%q auto=%v err=%v", note, auto, err)
	}
}

func TestOperatorCommandSystemEventsDoNotMoveActivity(t *testing.T) {
	st := schemaStore(t, "opcmd_auto")
	lane, job := "opcmd-auto-lane", "opcmd-auto-job"
	task := operatorCommandFixture(t, st, lane, job)
	ctx := t.Context()
	command := recordWorkerMessage(t, st, task.ID, "opcmd-auto-key", "지시", OperatorCommandIntentSteer)
	events, err := st.ListRelayEvents(ctx, lane, true, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("relay: %+v %v", events, err)
	}
	if _, err = st.MarkRelayEventDelivered(ctx, events[0].ID, "example-host", "pane-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateAcked, By: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateDone, By: "node-a"}); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	rows, err := st.pool.Query(ctx, `SELECT source_ref, auto FROM task_events WHERE task_id=$1 AND source='operator-command'`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var auto bool
		if err = rows.Scan(&ref, &auto); err != nil {
			t.Fatal(err)
		}
		got[ref] = auto
	}
	want := map[string]bool{
		"opcmd-auto-key:recorded":  false,
		"opcmd-auto-key:delivered": true,
		"opcmd-auto-key:acked":     false,
		"opcmd-auto-key:done":      false,
	}
	if len(got) != len(want) {
		t.Fatalf("events: %v", got)
	}
	for ref, auto := range want {
		if value, ok := got[ref]; !ok || value != auto {
			t.Fatalf("event %s auto=%v want %v (all %v)", ref, value, auto, got)
		}
	}
}

func TestOperatorCommandDeliveryAfterExpiry(t *testing.T) {
	st := schemaStore(t, "opcmd_latedeliver")
	lane, job := "opcmd-latedeliver-lane", "opcmd-latedeliver-job"
	task := operatorCommandFixture(t, st, lane, job)
	ctx := t.Context()
	command := recordWorkerMessage(t, st, task.ID, "opcmd-latedeliver-key", "지시", OperatorCommandIntentSteer)
	if _, err := st.pool.Exec(ctx, `UPDATE operator_commands SET expires_at=now()-interval '1 minute' WHERE id=$1`, command.ID); err != nil {
		t.Fatal(err)
	}
	// The drainer polls relay_events without touching operator_commands; this
	// is the path that used to deliver a stale instruction as 'delivered'.
	events, err := st.ListRelayEvents(ctx, lane, true, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("relay: %+v %v", events, err)
	}
	if _, err = st.MarkRelayEventDelivered(ctx, events[0].ID, "example-host", "pane-1"); err != nil {
		t.Fatal(err)
	}
	got := commandByID(t, st, command.ID)
	if got.State != OperatorCommandStateExpired || got.DeliveredAt != nil || got.FinishedAt == nil {
		t.Fatalf("late delivery: %+v", got)
	}
	var note string
	var auto bool
	if err := st.pool.QueryRow(ctx, `SELECT note, auto FROM task_events WHERE task_id=$1 AND source_ref=$2`, task.ID, "opcmd-latedeliver-key:delivered").Scan(&note, &auto); err != nil || !strings.HasPrefix(note, "만료 후 전달 · ") || !auto {
		t.Fatalf("late delivery event: note=%q auto=%v err=%v", note, auto, err)
	}
}

func TestOperatorCommandLateDeliveryOfSuperseded(t *testing.T) {
	st := schemaStore(t, "opcmd_latesuper")
	lane, job := "opcmd-latesuper-lane", "opcmd-latesuper-job"
	task := operatorCommandFixture(t, st, lane, job)
	ctx := t.Context()
	first := recordWorkerMessage(t, st, task.ID, "opcmd-latesuper-a", "첫 명령", OperatorCommandIntentSteer)
	recordWorkerMessage(t, st, task.ID, "opcmd-latesuper-b", "두 번째 명령", OperatorCommandIntentSteer)
	// A drainer fetched A before the replacement closed its relay row; it
	// delivers A anyway. The ladder keeps 대체됨 and records the late delivery.
	var relayID int64
	if err := st.pool.QueryRow(ctx, `SELECT id FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, lane, "opcmd-latesuper-a").Scan(&relayID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkRelayEventDelivered(ctx, relayID, "example-host", "pane-1"); err != nil {
		t.Fatal(err)
	}
	if got := commandByID(t, st, first.ID); got.State != OperatorCommandStateSuperseded {
		t.Fatalf("superseded command moved: %+v", got)
	}
	var note string
	if err := st.pool.QueryRow(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND source_ref=$2`, task.ID, "opcmd-latesuper-a:delivered").Scan(&note); err != nil || !strings.HasPrefix(note, "대체 후 전달 · ") {
		t.Fatalf("late delivery event: note=%q err=%v", note, err)
	}
}

func TestOperatorCommandReceiptAtDeadlineRecordsReport(t *testing.T) {
	st := schemaStore(t, "opcmd_deadline")
	task := operatorCommandFixture(t, st, "opcmd-deadline-lane", "opcmd-deadline-job")
	ctx := t.Context()
	command := recordWorkerMessage(t, st, task.ID, "opcmd-deadline-key", "지시", OperatorCommandIntentSteer)
	if _, err := st.pool.Exec(ctx, `UPDATE operator_commands SET expires_at=now()-interval '1 minute' WHERE id=$1`, command.ID); err != nil {
		t.Fatal(err)
	}
	// No list/sweep runs first: the receipt itself discovers the deadline.
	done, err := st.RecordOperatorCommandReceipt(ctx, command.ID, OperatorCommandReceiptInput{State: OperatorCommandStateDone, By: "node-a"})
	if err != nil || done.State != OperatorCommandStateExpired {
		t.Fatalf("deadline receipt: %+v %v", done, err)
	}
	var note, by string
	if err := st.pool.QueryRow(ctx, `SELECT note, "by" FROM task_events WHERE task_id=$1 AND source_ref=$2`, task.ID, "opcmd-deadline-key:done").Scan(&note, &by); err != nil || !strings.HasPrefix(note, "만료 후 보고 · ") || by != "executor:node-a" {
		t.Fatalf("late report: note=%q by=%q err=%v", note, by, err)
	}
	var deliveredTo string
	if err := st.pool.QueryRow(ctx, `SELECT delivered_to FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, "opcmd-deadline-lane", "opcmd-deadline-key").Scan(&deliveredTo); err != nil || deliveredTo != "cancelled:expired" {
		t.Fatalf("deadline relay: %q %v", deliveredTo, err)
	}
}

func TestOperatorCommandKeyCollisionWithLaneEvent(t *testing.T) {
	st := schemaStore(t, "opcmd_collision")
	lane, job := "opcmd-collision-lane", "opcmd-collision-job"
	task := operatorCommandFixture(t, st, lane, job)
	ctx := t.Context()
	// Another producer already owns this event id on the lane; attaching the
	// command to that row would deliver the wrong text and cancel it later.
	if _, _, err := st.AppendRelayEvent(ctx, RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: "opcmd-collision-key", Text: "다른 생산자의 이벤트"}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(WorkerMessagePayload{Text: "지시", Intent: OperatorCommandIntentSteer})
	_, err := st.RecordOperatorCommand(ctx, OperatorCommandInput{
		CommandKey: "opcmd-collision-key", Kind: OperatorCommandKindWorkerMessage, TaskID: &task.ID,
		Payload: payload, Actor: "operator:test", Channel: "console",
	})
	if !errors.Is(err, ErrOperatorCommandInvalid) {
		t.Fatalf("collision: %v", err)
	}
	var commands int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM operator_commands WHERE task_id=$1`, task.ID).Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("rolled back commands=%d err=%v", commands, err)
	}
	var text string
	if err := st.pool.QueryRow(ctx, `SELECT text FROM relay_events WHERE owner_lane=$1 AND event_id=$2`, lane, "opcmd-collision-key").Scan(&text); err != nil || text != "다른 생산자의 이벤트" {
		t.Fatalf("relay text=%q err=%v", text, err)
	}
}

func TestOperatorCommandReplaySurvivesJobEnd(t *testing.T) {
	st := schemaStore(t, "opcmd_replay")
	lane, job := "opcmd-replay-lane", "opcmd-replay-job"
	task := operatorCommandFixture(t, st, lane, job)
	first := recordWorkerMessage(t, st, task.ID, "opcmd-replay-key", "지시", OperatorCommandIntentSteer)
	// The lost-response retry arrives after the worker's job ended: it must
	// return the original command, not 409 no_executor.
	if _, err := st.pool.Exec(t.Context(), `UPDATE jobs SET state='completed' WHERE job_id=$1`, job); err != nil {
		t.Fatal(err)
	}
	again := recordWorkerMessage(t, st, task.ID, "opcmd-replay-key", "지시", OperatorCommandIntentSteer)
	if again.ID != first.ID {
		t.Fatalf("replay after job end: %+v", again)
	}
	var commands int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM operator_commands WHERE task_id=$1`, task.ID).Scan(&commands); err != nil || commands != 1 {
		t.Fatalf("commands=%d err=%v", commands, err)
	}
}

func TestOperatorCommandReceiptRejectsSecretErrorCode(t *testing.T) {
	st := schemaStore(t, "opcmd_secret")
	task := operatorCommandFixture(t, st, "opcmd-secret-lane", "opcmd-secret-job")
	command := recordWorkerMessage(t, st, task.ID, "opcmd-secret-key", "지시", OperatorCommandIntentSteer)
	_, err := st.RecordOperatorCommandReceipt(t.Context(), command.ID, OperatorCommandReceiptInput{
		State: OperatorCommandStateFailed, ErrorCode: "ghp_" + strings.Repeat("a", 36),
	})
	if _, ok := guard.SecretPattern(err); !ok {
		t.Fatalf("secret error_code accepted or wrong error: %v", err)
	}
	if got := commandByID(t, st, command.ID); got.State != OperatorCommandStateRecorded {
		t.Fatalf("refused receipt changed state: %+v", got)
	}
}

func TestOperatorCommandListOrderingEndsInID(t *testing.T) {
	st := schemaStore(t, "opcmd_list")
	task := operatorCommandFixture(t, st, "opcmd-list-lane", "opcmd-list-job")
	ctx := t.Context()
	at := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	ids := []int64{}
	for i, key := range []string{"opcmd-list-a", "opcmd-list-b", "opcmd-list-c"} {
		payload, _ := json.Marshal(WorkerMessagePayload{Text: "지시", Intent: OperatorCommandIntentSteer})
		command, err := st.RecordOperatorCommand(ctx, OperatorCommandInput{CommandKey: key, Kind: OperatorCommandKindWorkerMessage, TaskID: &task.ID, Payload: payload, Actor: "operator:test", Channel: "console"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, command.ID)
		if _, err := st.pool.Exec(ctx, `UPDATE operator_commands SET created_at=$1 WHERE id=$2`, at.Add(time.Duration(i)*time.Minute), command.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Same created_at for all three: the tie must break on id, ascending.
	if _, err := st.pool.Exec(ctx, `UPDATE operator_commands SET created_at=$1 WHERE task_id=$2`, at, task.ID); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListOperatorCommandsByTask(ctx, task.ID, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].ID != ids[2] || list[1].ID != ids[1] || list[2].ID != ids[0] {
		t.Fatalf("task list order: %+v", list)
	}
	page, err := st.ListOperatorCommands(ctx, "opcmd-list-lane", "", 0, 10)
	if err != nil || len(page) != 3 || page[0].ID != ids[0] || page[2].ID != ids[2] {
		t.Fatalf("executor list order: %+v %v", page, err)
	}
	after, err := st.ListOperatorCommands(ctx, "opcmd-list-lane", "", ids[0], 10)
	if err != nil || len(after) != 2 || after[0].ID != ids[1] {
		t.Fatalf("after cursor: %+v %v", after, err)
	}
}

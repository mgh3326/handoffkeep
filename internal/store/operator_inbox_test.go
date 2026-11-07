package store

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestOperatorInboxMigration builds the two tables from a fresh schema and
// checks the columns and the action guard.
func TestOperatorInboxMigration(t *testing.T) {
	ctx := t.Context()
	pool, _ := testdb.SchemaPool(t, "inbox_migration")
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	columns := func(table string) []string {
		rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out = append(out, name)
		}
		return out
	}
	wantState := []string{"item_key", "read_at", "done_at", "snoozed_until", "actor", "updated_at"}
	if got := columns("operator_inbox_state"); len(got) != len(wantState) {
		t.Fatalf("operator_inbox_state columns %v, want %v", got, wantState)
	} else {
		for i := range wantState {
			if got[i] != wantState[i] {
				t.Fatalf("operator_inbox_state columns %v, want %v", got, wantState)
			}
		}
	}
	if got := columns("operator_inbox_events"); len(got) != 11 {
		t.Fatalf("operator_inbox_events has %d columns, want 11: %v", len(got), got)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=$1)`, operatorInboxMigrationID).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("migration id not recorded: %v %v", recorded, err)
	}
	// The ledger refuses an action the store never writes.
	if _, err := pool.Exec(ctx, `INSERT INTO operator_inbox_events(event_key,item_key,action,actor,at) VALUES('k','task:1','bogus','a',now())`); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func inboxKeyTask(id int64) string { return "task:" + strconv.FormatInt(id, 10) }

func TestApplyInboxStateAndReplay(t *testing.T) {
	s := schemaStore(t, "inbox_apply")
	ctx := t.Context()
	key := inboxKeyTask(12)
	result, err := s.ApplyInbox(ctx, []string{key}, InboxActionRead, nil, "operator:test", "event-one")
	if err != nil || len(result.Items) != 1 || result.Items[0].Err != nil {
		t.Fatalf("read: %+v %v", result, err)
	}
	if result.Items[0].State.ReadAt == nil {
		t.Fatal("read did not record a read time")
	}
	first := result.Items[0].State.ReadAt

	// The same event key replays the first result: read_at does not move.
	replay, err := s.ApplyInbox(ctx, []string{key}, InboxActionRead, nil, "operator:test", "event-one")
	if err != nil || len(replay.Items) != 1 || replay.Items[0].Err != nil {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if first == nil || replay.Items[0].State.ReadAt == nil || !replay.Items[0].State.ReadAt.Equal(*first) {
		t.Fatalf("replay moved read_at: first %v replay %v", first, replay.Items[0].State.ReadAt)
	}
	var events int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_inbox_events WHERE event_key='event-one'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("event rows %d err %v", events, err)
	}

	if _, err := s.ApplyInbox(ctx, []string{key}, InboxActionUnread, nil, "operator:test", "event-two"); err != nil {
		t.Fatal(err)
	}
	states, err := s.InboxStates(ctx, []string{key, inboxKeyTask(13)})
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := states[key]; !ok || x.ReadAt != nil || x.DoneAt != nil {
		t.Fatalf("unread state %+v present %v", x, ok)
	}
	if _, ok := states[inboxKeyTask(13)]; ok {
		t.Fatal("a key with no write must not be reported")
	}

	if _, err := s.ApplyInbox(ctx, []string{key}, InboxActionDone, nil, "operator:test", "event-three"); err != nil {
		t.Fatal(err)
	}
	states, _ = s.InboxStates(ctx, []string{key})
	if states[key].DoneAt == nil {
		t.Fatal("done did not record a done time")
	}
}

func TestApplyInboxValidates(t *testing.T) {
	s := schemaStore(t, "inbox_validate")
	ctx := t.Context()
	// A bad item key is refused per item: the rest of the batch still applies.
	for _, tc := range []struct {
		name, key string
		want      error
	}{
		{"key", "conversation:1", ErrInvalidInboxItem},
		{"trailing", "task:12x", ErrInvalidInboxItem},
		{"zero id", "task:0", ErrInvalidInboxItem},
		{"deploy", "deploy:77", nil},
		{"deploy service", "deploy:handoffkeep:77", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := s.ApplyInbox(ctx, []string{tc.key}, InboxActionRead, nil, "operator:test", "e-"+tc.name)
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			if len(result.Items) != 1 || !errors.Is(result.Items[0].Err, tc.want) {
				t.Fatalf("item outcome %+v, want %v", result.Items, tc.want)
			}
		})
	}
	// An envelope problem refuses the whole call.
	for _, tc := range []struct {
		name, actor, event string
		want               error
	}{
		{"actor", "", "e", ErrInvalidInboxWrite},
		{"event", "operator:test", "", ErrInvalidInboxWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.ApplyInbox(ctx, []string{"task:1"}, InboxActionRead, nil, tc.actor, tc.event); !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := s.ApplyInbox(ctx, []string{"task:1"}, "bogus", nil, "operator:test", "e"); !errors.Is(err, ErrInvalidInboxWrite) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err := s.ApplyInbox(ctx, []string{"task:1"}, InboxActionSnooze, nil, "operator:test", "e"); !errors.Is(err, ErrInvalidInboxSnooze) {
		t.Fatalf("missing snooze time: %v", err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := s.ApplyInbox(ctx, []string{"task:1"}, InboxActionSnooze, &past, "operator:test", "e"); !errors.Is(err, ErrInvalidInboxSnooze) {
		t.Fatalf("past snooze: %v", err)
	}
	far := time.Now().UTC().Add(MaxInboxSnooze + time.Hour)
	if _, err := s.ApplyInbox(ctx, []string{"task:1"}, InboxActionSnooze, &far, "operator:test", "e"); !errors.Is(err, ErrInvalidInboxSnooze) {
		t.Fatalf("far snooze: %v", err)
	}
}

func TestApplyInboxBatchCapAndUndo(t *testing.T) {
	s := schemaStore(t, "inbox_batch")
	ctx := t.Context()
	keys := make([]string, 0, InboxBatchLimit+2)
	for i := 0; i < InboxBatchLimit+2; i++ {
		keys = append(keys, inboxKeyTask(int64(i+1)))
	}
	result, err := s.ApplyInbox(ctx, keys, InboxActionDone, nil, "operator:test", "bulk")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != InboxBatchLimit || result.Skipped != 2 {
		t.Fatalf("applied %d skipped %d, want %d and 2", len(result.Items), result.Skipped, InboxBatchLimit)
	}
	states, err := s.InboxStates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != InboxBatchLimit {
		t.Fatalf("stored %d states, want %d", len(states), InboxBatchLimit)
	}
	restored, err := s.UndoInbox(ctx, "bulk", "operator:test")
	if err != nil || len(restored) != InboxBatchLimit {
		t.Fatalf("undo: %d items %v", len(restored), err)
	}
	states, _ = s.InboxStates(ctx, keys)
	if len(states) != 0 {
		t.Fatalf("undo left %d states", len(states))
	}
	// A second undo of the same write is a no-op.
	again, err := s.UndoInbox(ctx, "bulk", "operator:test")
	if err != nil || len(again) != 0 {
		t.Fatalf("second undo: %d items %v", len(again), err)
	}
}

func TestInboxSnoozeDecisionInSameTransaction(t *testing.T) {
	s := schemaStore(t, "inbox_decision")
	ctx := t.Context()
	id := insertAttentionRequest(t, s, attentionRequest{ref: "snooze-me"})
	key := "decision:request:" + strconv.FormatInt(id, 10)
	until := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	result, err := s.ApplyInbox(ctx, []string{key}, InboxActionSnooze, &until, "operator:test", "snooze-one")
	if err != nil || len(result.Items) != 1 || result.Items[0].Err != nil {
		t.Fatalf("snooze: %+v %v", result, err)
	}
	var requestSnooze *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT snoozed_until FROM decision_requests WHERE id=$1`, id).Scan(&requestSnooze); err != nil {
		t.Fatal(err)
	}
	if requestSnooze == nil || !requestSnooze.Equal(until) {
		t.Fatalf("request snooze %v, want %v", requestSnooze, until)
	}
	if _, err := s.UndoInbox(ctx, "snooze-one", "operator:test"); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT snoozed_until FROM decision_requests WHERE id=$1`, id).Scan(&requestSnooze); err != nil {
		t.Fatal(err)
	}
	if requestSnooze != nil {
		t.Fatalf("undo left the request snoozed until %v", requestSnooze)
	}

	// A request that is no longer open refuses the whole item, so the inbox
	// row is not written either: both writes share one transaction.
	closed := insertAttentionRequest(t, s, attentionRequest{ref: "closed", status: DecisionStatusAnswered})
	closedKey := "decision:request:" + strconv.FormatInt(closed, 10)
	failed, err := s.ApplyInbox(ctx, []string{closedKey}, InboxActionSnooze, &until, "operator:test", "snooze-two")
	if err != nil || len(failed.Items) != 1 || !errors.Is(failed.Items[0].Err, ErrDecisionRecordNotOpen) {
		t.Fatalf("closed request: %+v %v", failed, err)
	}
	states, err := s.InboxStates(ctx, []string{closedKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("failed snooze left inbox state %+v", states)
	}
}

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func relaneLane(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func relaneTask(t *testing.T, s *Store, lane, title string) Task {
	t.Helper()
	x, err := s.CreateTask(context.Background(), Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "relane-test"})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A relane moves only lane and updated_at. Every other column — state,
// priority, refs, claimant, parent lane, title, kind, body_doc — is asserted
// unchanged, and the append-only event records actor, lanes, and reason.
func TestRelaneTaskMovesLaneAndRecordsEvent(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	from, to := relaneLane(t, "relane-from"), relaneLane(t, "relane-to")
	x, err := s.CreateTask(ctx, Task{
		Lane: from, ParentLane: "director-1", Title: "moving task", Kind: "fix", Priority: 7,
		Refs: TaskRefs{PR: "org/repo#123", JobID: "job-9"}, CreatedBy: "relane-test", BodyDoc: "doc/keep-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	relaneTask(t, s, to, "lane anchor") // to must be a lane some row uses
	claimed, err := s.ClaimTask(ctx, x.ID, "captain")
	if err != nil {
		t.Fatal(err)
	}
	moved, changed, err := s.RelaneTask(ctx, x.ID, to, "tester-1", "triage migration", false)
	if err != nil || !changed {
		t.Fatalf("relane changed=%v err=%v", changed, err)
	}
	if moved.Lane != to || moved.State != "claimed" || moved.ClaimedBy != "captain" ||
		moved.Priority != 7 || moved.ParentLane != "director-1" || moved.Title != "moving task" ||
		moved.Kind != "fix" || moved.BodyDoc != "doc/keep-me" || moved.CreatedBy != "relane-test" ||
		moved.Refs.PR != "org/repo#123" || moved.Refs.JobID != "job-9" {
		t.Fatalf("moved=%+v", moved)
	}
	if !moved.UpdatedAt.After(claimed.UpdatedAt) {
		t.Fatalf("updated_at not advanced: %v <= %v", moved.UpdatedAt, claimed.UpdatedAt)
	}
	got, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found || len(got.Events) != 2 {
		t.Fatalf("events=%v found=%v err=%v", got.Events, found, err)
	}
	if got.Events[0].Kind != TaskEventTransition || got.Events[0].To != "claimed" {
		t.Fatalf("transition event rewritten: %+v", got.Events[0])
	}
	last := got.Events[1]
	if last.Kind != TaskEventRelane || last.From != from || last.To != to ||
		last.By != "tester-1" || last.Note != "triage migration" {
		t.Fatalf("relane event=%+v", last)
	}
	if last.Refs == nil || last.Refs.PR != "org/repo#123" || last.Refs.JobID != "job-9" {
		t.Fatalf("relane event refs=%+v", last.Refs)
	}
	if last.At.IsZero() {
		t.Fatal("relane event has no timestamp")
	}
}

// Rerunning a batch relane is safe: same-lane is a successful no-op that
// writes no event, so history is never falsified by retries.
func TestRelaneTaskSameLaneIsNoop(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	x := relaneTask(t, s, relaneLane(t, "relane-same"), "already here")
	moved, changed, err := s.RelaneTask(ctx, x.ID, x.Lane, "tester", "noop", false)
	if err != nil || changed || moved.Lane != x.Lane {
		t.Fatalf("changed=%v moved=%+v err=%v", changed, moved, err)
	}
	got, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found || len(got.Events) != 0 {
		t.Fatalf("noop wrote history: events=%+v", got.Events)
	}
}

// Terminal rows are immutable history: merged and dropped tasks refuse a
// relane even with allow-new-lane, and neither lane nor events change.
func TestRelaneTaskTerminalRejected(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane, to := relaneLane(t, "relane-term"), relaneLane(t, "relane-term-to")
	relaneTask(t, s, to, "lane anchor")
	for _, state := range []string{"merged", "dropped"} {
		id := seedTaskRow(t, pool, lane, "closed "+state, state)
		for _, allow := range []bool{false, true} {
			if _, _, err := s.RelaneTask(ctx, id, to, "tester", "move closed", allow); !errors.Is(err, ErrTaskTerminal) {
				t.Fatalf("state=%s allow=%v err=%v, want task_terminal", state, allow, err)
			}
		}
		got, found, err := s.GetTask(ctx, id)
		if err != nil || !found || got.Lane != lane || got.State != state || len(got.Events) != 0 {
			t.Fatalf("state=%s after=%+v events=%d", state, got, len(got.Events))
		}
	}
}

// A mistyped lane is refused: the target must be a lane some current row
// already uses, unless the caller opts into a new lane explicitly.
func TestRelaneTaskUnknownLaneRejected(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	from := relaneLane(t, "relane-typo")
	x := relaneTask(t, s, from, "typo probe")
	typo := relaneLane(t, "direktor")
	if _, _, err := s.RelaneTask(ctx, x.ID, typo, "tester", "typo", false); !errors.Is(err, ErrTaskLaneUnknown) {
		t.Fatalf("err=%v, want unknown_lane", err)
	}
	got, _, _ := s.GetTask(ctx, x.ID)
	if got.Lane != from || len(got.Events) != 0 {
		t.Fatalf("rejected relane mutated task: %+v", got)
	}
	// Opt-in override accepts the same name.
	moved, changed, err := s.RelaneTask(ctx, x.ID, typo, "tester", "intentional new lane", true)
	if err != nil || !changed || moved.Lane != typo {
		t.Fatalf("allow-new-lane changed=%v lane=%q err=%v", changed, moved.Lane, err)
	}
	// Lanes are known from any lane-bearing row, not only tasks.lane: a lane
	// that appears only as a parent_lane and one that appears only as a
	// relay owner both pass without the override.
	parentOnly := relaneLane(t, "relane-parent-only")
	if _, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "relane-child"), ParentLane: parentOnly, Title: "child", Kind: "implement", CreatedBy: "relane-test"}); err != nil {
		t.Fatal(err)
	}
	y := relaneTask(t, s, from, "parent lane target")
	if _, changed, err := s.RelaneTask(ctx, y.ID, parentOnly, "tester", "known via parent", false); err != nil || !changed {
		t.Fatalf("parent-only lane changed=%v err=%v", changed, err)
	}
	relayOnly := relaneLane(t, "relane-relay-only")
	if _, err := pool.Exec(ctx, `INSERT INTO relay_events(kind,job_id,owner_lane,received_at) VALUES('job.completed','j-1',$1,now())`, relayOnly); err != nil {
		t.Fatal(err)
	}
	z := relaneTask(t, s, from, "relay lane target")
	if _, changed, err := s.RelaneTask(ctx, z.ID, relayOnly, "tester", "known via relay", false); err != nil || !changed {
		t.Fatalf("relay-only lane changed=%v err=%v", changed, err)
	}
}

func TestRelaneTaskNotFoundAndValidation(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	lane, to := relaneLane(t, "relane-val"), relaneLane(t, "relane-val-to")
	x := relaneTask(t, s, lane, "validation probe")
	relaneTask(t, s, to, "lane anchor")
	if _, _, err := s.RelaneTask(ctx, x.ID+999999, to, "tester", "missing", false); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err=%v, want task_not_found", err)
	}
	for _, tc := range []struct {
		to, by, note string
	}{
		{"", "tester", "r"},                                        // empty target
		{"bad lane", "tester", "r"},                                // space in lane
		{to, "", "r"},                                              // empty actor
		{to, "tester", ""},                                         // empty reason
		{to, "tester", "   "},                                      // whitespace reason
		{to, "tester", strings.Repeat("a", MaxBytes+1)},            // oversized note
		{strings.Repeat("l", 129), "tester", "r"},                  // oversized lane
		{to, strings.Repeat("b", 129), "r"},                        // oversized actor
		{to, "tester", "token: Bearer abcdefghijklmnopqrstuvwxyz"}, // secret-like note
	} {
		if _, _, err := s.RelaneTask(ctx, x.ID, tc.to, tc.by, tc.note, true); err == nil {
			t.Fatalf("RelaneTask(to=%q by=%q note=%q) succeeded, want refusal", tc.to, tc.by, tc.note)
		}
	}
	got, _, _ := s.GetTask(ctx, x.ID)
	if got.Lane != lane || len(got.Events) != 0 {
		t.Fatalf("validation failures mutated task: %+v", got)
	}
}

// A lane named like a state is legal input, and must never read back as a
// state transition: merged-task reporting is fed only by kind='transition'.
func TestRelaneTaskStateNamedLaneNotATransition(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	relaneTask(t, s, "merged", "lane anchor")
	x := relaneTask(t, s, relaneLane(t, "relane-real"), "looks like a merge")
	if _, _, err := s.RelaneTask(ctx, x.ID, "merged", "tester", "lane called merged", false); err != nil {
		t.Fatal(err)
	}
	merged, err := s.ListMergedTaskEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range merged {
		if e.TaskID == x.ID {
			t.Fatalf("relane to lane 'merged' counted as a merge event")
		}
	}
	got, found, _ := s.GetTask(ctx, x.ID)
	if !found || got.State != "backlog" || got.Lane != "merged" {
		t.Fatalf("got=%+v found=%v", got, found)
	}
	if len(got.Events) != 1 || got.Events[0].Kind != TaskEventRelane {
		t.Fatalf("events=%+v", got.Events)
	}
}

// Relane and transition take the same row lock, so they serialize: a
// concurrent pair on one task both commit, and the event order is lock order.
func TestRelaneTaskConcurrentTransition(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	ids := make([]int64, 0, n)
	targets := map[int64]string{}
	for i := 0; i < n; i++ {
		from, to := relaneLane(t, "relane-race-a"), relaneLane(t, "relane-race-b")
		x := relaneTask(t, s, from, "race")
		relaneTask(t, s, to, "anchor")
		if _, err := s.ClaimTask(ctx, x.ID, "captain"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, x.ID)
		targets[x.ID] = to
		wg.Add(2)
		go func(id int64, to string) {
			defer wg.Done()
			if _, _, err := s.RelaneTask(ctx, id, to, "racer-a", "race", false); err != nil {
				errs <- err
			}
		}(x.ID, to)
		go func(id int64) {
			defer wg.Done()
			if _, err := s.TransitionTask(ctx, id, "in_progress", "racer-b", "", nil); err != nil {
				errs <- err
			}
		}(x.ID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent op failed: %v", err)
	}
	// Every task ends in_progress in its target lane with exactly one claim,
	// one transition, and one relane event — no lost update, no phantom row.
	for _, id := range ids {
		got, found, err := s.GetTask(ctx, id)
		if err != nil || !found {
			t.Fatalf("task %d: found=%v err=%v", id, found, err)
		}
		if got.Lane != targets[id] || got.State != "in_progress" || got.ClaimedBy != "captain" {
			t.Fatalf("task %d: %+v", id, got)
		}
		kinds := map[string]int{}
		for _, e := range got.Events {
			kinds[e.Kind]++
			if e.Kind == TaskEventRelane && (e.From == "" || e.To != targets[id]) {
				t.Fatalf("task %d relane event=%+v", id, e)
			}
		}
		if kinds[TaskEventTransition] != 2 || kinds[TaskEventRelane] != 1 || len(got.Events) != 3 {
			t.Fatalf("task %d events=%+v", id, got.Events)
		}
	}
}

// The append-only trigger covers relane rows like transition rows: UPDATE,
// DELETE, and TRUNCATE all fail.
func TestRelaneTaskEventAppendOnly(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	from, to := relaneLane(t, "relane-ao"), relaneLane(t, "relane-ao-to")
	x := relaneTask(t, s, from, "append only")
	relaneTask(t, s, to, "anchor")
	if _, _, err := s.RelaneTask(ctx, x.ID, to, "tester", "move", false); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE task_events SET note='tampered' WHERE task_id=$1`,
		`DELETE FROM task_events WHERE task_id=$1`,
	} {
		if _, err := pool.Exec(ctx, q, x.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%q err=%v, want append-only refusal", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE task_events`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("TRUNCATE err=%v, want append-only refusal", err)
	}
}

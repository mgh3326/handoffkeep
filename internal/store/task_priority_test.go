package store

import (
	"context"
	"errors"
	"testing"
)

func TestSetTaskPriority(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{Lane: taskProjectLane(t), Title: "priority", Kind: "implement", CreatedBy: "test", Project: projectPtr("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetTaskPriority(ctx, x.ID, -1, "operator:test", "", "invalid"); !errors.Is(err, ErrInvalidTaskPriority) {
		t.Fatalf("negative: %v", err)
	}
	first, err := s.SetTaskPriority(ctx, x.ID, 2, "operator:test", "reason", "one")
	if err != nil || first.Previous != x.Priority || first.Priority != 2 {
		t.Fatalf("first: %+v %v", first, err)
	}
	if _, err = s.SetTaskPriority(ctx, x.ID, 3, "operator:test", "", "two"); err != nil {
		t.Fatal(err)
	}
	replay, err := s.SetTaskPriority(ctx, x.ID, 4, "operator:test", "", "one")
	if err != nil || replay != first {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var count int
	var actor, note string
	if err = pool.QueryRow(ctx, `SELECT count(*),min("by"),min(note) FROM task_events WHERE task_id=$1 AND source='ui-priority' AND source_ref='one'`, x.ID).Scan(&count, &actor, &note); err != nil || count != 1 || actor != "operator:test" || note != "priority 0 → 2: reason" {
		t.Fatalf("event: %d %q %q %v", count, actor, note, err)
	}
	if _, err = s.TransitionTask(ctx, x.ID, "dropped", "test", "closed", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetTaskPriority(ctx, x.ID, 1, "operator:test", "", "three"); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("terminal: %v", err)
	}
}

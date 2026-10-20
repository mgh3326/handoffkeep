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

func taskRebindFixture(t *testing.T, state string) (*Store, Task) {
	t.Helper()
	s, _ := searchTestStore(t)
	ctx := context.Background()
	project := projectPtr(testProjectName)
	task, err := s.CreateTask(ctx, Task{
		Lane: fmt.Sprintf("rebind-%d", time.Now().UnixNano()), Title: "rebind fixture",
		Kind: "implement", CreatedBy: "rebind-test", Project: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, task.ID, "old-builder", "job-old", ""); err != nil {
		t.Fatal(err)
	}
	if state == "in_progress" {
		if _, err := s.TransitionTask(ctx, task.ID, state, "old-builder", "", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	return s, task
}

func TestRebindTaskJobChangesActiveLinkAndRecordsEvent(t *testing.T) {
	s, task := taskRebindFixture(t, "claimed")
	ctx := context.Background()
	first, err := s.RebindTaskJob(ctx, task.ID, "job-old", "job-new", "new-builder", "builder takeover")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "claimed" || first.Refs.JobID != "job-new" || first.ClaimedBy != "new-builder" {
		t.Fatalf("rebound task=%+v", first)
	}
	got, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("get task found=%v err=%v", found, err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events=%+v, want claim plus one rebind", got.Events)
	}
	event := got.Events[1]
	if event.Kind != TaskEventTransition || event.From != "claimed" || event.To != "claimed" ||
		event.By != "new-builder" || event.Note != "builder takeover" || event.Rebind == nil ||
		event.Rebind.FromJob != "job-old" || event.Rebind.ToJob != "job-new" ||
		event.Rebind.By != "new-builder" || event.Rebind.Reason != "builder takeover" ||
		event.Refs == nil || event.Refs.JobID != "job-new" {
		t.Fatalf("rebind event=%+v", event)
	}

	replayed, err := s.RebindTaskJob(ctx, task.ID, "job-old", "job-new", "new-builder", "retry")
	if err != nil {
		t.Fatalf("same rebind replay: %v", err)
	}
	got, _, err = s.GetTask(ctx, task.ID)
	if err != nil || len(got.Events) != 2 || !replayed.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("replay changed task: updated=%v/%v events=%d err=%v", first.UpdatedAt, replayed.UpdatedAt, len(got.Events), err)
	}
}

func TestRebindTaskJobAllowsInProgressAndRejectsMismatches(t *testing.T) {
	s, task := taskRebindFixture(t, "in_progress")
	ctx := context.Background()
	got, err := s.RebindTaskJob(ctx, task.ID, "job-old", "job-new", "new-builder", "takeover")
	if err != nil || got.State != "in_progress" {
		t.Fatalf("in-progress rebind=%+v err=%v", got, err)
	}
	if _, err := s.RebindTaskJob(ctx, task.ID, "job-old", "job-next", "third-builder", "stale"); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), `current job_id is "job-new"`) {
		t.Fatalf("stale rebind err=%v, want conflict naming current job", err)
	}
	if _, err := s.RebindTaskJob(ctx, task.ID, "wrong-old", "job-new", "new-builder", "wrong source"); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), `current job_id is "job-new"`) {
		t.Fatalf("wrong-source replay err=%v, want conflict naming current job", err)
	}
	after, _, err := s.GetTask(ctx, task.ID)
	if err != nil || after.Refs.JobID != "job-new" || after.ClaimedBy != "new-builder" || len(after.Events) != 3 {
		t.Fatalf("refused rebind changed task: task=%+v err=%v", after, err)
	}
}

func TestRebindTaskJobRejectsNonActiveStates(t *testing.T) {
	for _, tc := range []struct {
		state string
		path  []string
	}{
		{state: "hold", path: []string{"hold"}},
		{state: "needs_decision", path: []string{"needs_decision"}},
		{state: "dropped", path: []string{"dropped"}},
		{state: "verifying", path: []string{"in_progress", "verifying"}},
		{state: "join", path: []string{"in_progress", "join"}},
		{state: "merged", path: []string{"in_progress", "verifying", "merged"}},
	} {
		t.Run(tc.state, func(t *testing.T) {
			s, task := taskRebindFixture(t, "claimed")
			ctx := context.Background()
			for _, state := range tc.path {
				note := ""
				if state == "needs_decision" {
					note = "operator decision needed"
				}
				if _, err := s.TransitionTask(ctx, task.ID, state, "old-builder", note, nil, ""); err != nil {
					t.Fatalf("transition to %s: %v", state, err)
				}
			}
			before, _, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RebindTaskJob(ctx, task.ID, "job-old", "job-new", "new-builder", "takeover"); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), `current job_id is "job-old"`) {
				t.Fatalf("rebind err=%v, want conflict naming current job", err)
			}
			after, _, err := s.GetTask(ctx, task.ID)
			if err != nil || after.State != before.State || after.Refs.JobID != "job-old" || after.ClaimedBy != "old-builder" || len(after.Events) != len(before.Events) {
				t.Fatalf("refused rebind changed task: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestRebindTaskJobConcurrentTakeoversHaveOneWinner(t *testing.T) {
	s, task := taskRebindFixture(t, "claimed")
	ctx := context.Background()
	type result struct{ err error }
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, target := range []string{"job-new-a", "job-new-b"} {
		target := target
		go func() {
			ready.Done()
			ready.Wait()
			_, err := s.RebindTaskJob(ctx, task.ID, "job-old", target, target, "takeover")
			results <- result{err: err}
		}()
	}
	first, second := (<-results).err, (<-results).err
	wins, conflicts := 0, 0
	for _, err := range []error{first, second} {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrTaskConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected takeover error: %v", err)
		}
	}
	got, _, err := s.GetTask(ctx, task.ID)
	if err != nil || wins != 1 || conflicts != 1 || len(got.Events) != 2 || got.Refs.JobID == "job-old" {
		t.Fatalf("wins=%d conflicts=%d task=%+v err=%v", wins, conflicts, got, err)
	}
}

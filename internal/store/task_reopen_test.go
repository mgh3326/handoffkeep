package store

import (
	"context"
	"strings"
	"testing"
)

// This assertion kills a mutant that removes the reason guard, even without
// a database: a missing reason must be rejected before any storage access.
func TestReopenReasonRequired(t *testing.T) {
	for _, reason := range []string{"", " ", "\n\t"} {
		_, err := (*Store)(nil).ReopenTask(context.Background(), 1, "tester", reason)
		if err == nil || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
}

func TestReopenTerminalAuditAndRemerge(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for _, terminal := range []string{"merged", "dropped"} {
		x := relaneTask(t, s, relaneLane(t, "reopen"), "revert recovery")
		if _, err := s.ClaimTask(ctx, x.ID, "builder", "old-job", ""); err != nil {
			t.Fatal(err)
		}
		steps := []string{"dropped"}
		if terminal == "merged" {
			steps = []string{"in_progress", "join", "merged"}
		}
		for _, to := range steps {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "step", nil, ""); err != nil {
				t.Fatal(err)
			}
		}
		for _, to := range []string{"backlog", "claimed", "in_progress", "reopened"} {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "bypass", nil, "recovery"); err == nil {
				t.Fatalf("generic terminal exit to %s accepted", to)
			}
		}
		before, _, _ := s.GetTask(ctx, x.ID)
		for _, reason := range []string{"", " \n"} {
			if _, err := s.ReopenTask(ctx, x.ID, "tester", reason); err == nil {
				t.Fatal("reasonless reopen accepted")
			}
		}
		unchanged, _, _ := s.GetTask(ctx, x.ID)
		if unchanged.State != terminal || len(unchanged.Events) != len(before.Events) {
			t.Fatal("failed reopen mutated history")
		}
		got, err := s.ReopenTask(ctx, x.ID, "tester", "reverted after failed smoke")
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "reopened" || got.ClaimedBy != "" || got.Refs.JobID != "" {
			t.Fatalf("reopened=%+v", got)
		}
		got, _, _ = s.GetTask(ctx, x.ID)
		event := got.Events[len(got.Events)-1]
		if event.Kind != TaskEventTransition || event.From != terminal || event.To != "reopened" || event.By != "tester" || event.Note != "reverted after failed smoke" {
			t.Fatalf("audit=%+v", event)
		}
		if _, err := s.ReopenTask(ctx, x.ID, "tester", "again"); err == nil {
			t.Fatal("open task reopened")
		}
		events, err := s.ListMergedTaskEvents(ctx, 2000)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.TaskID == x.ID {
				t.Fatal("reopened task still shown as merged")
			}
		}
		next, err := s.NextTask(ctx, x.Lane, "new-builder", "new-job", "")
		if err != nil || next.ID != x.ID {
			t.Fatalf("next=%+v err=%v", next, err)
		}
		got, _, _ = s.GetTask(ctx, x.ID)
		if got.Events[len(got.Events)-1].From != "reopened" {
			t.Fatal("claim lost reopened source")
		}
		for _, to := range []string{"in_progress", "join", "merged"} {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "second cycle", &TaskRefs{PR: "example/project#2"}, ""); err != nil {
				t.Fatal(err)
			}
		}
		events, err = s.ListMergedTaskEvents(ctx, 2000)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, e := range events {
			if e.TaskID == x.ID {
				count++
				if e.Refs == nil || e.Refs.PR != "example/project#2" || !e.At.After(event.At) {
					t.Fatalf("latest merge=%+v", e)
				}
			}
		}
		if count != 1 {
			t.Fatalf("merge count=%d", count)
		}
	}
}

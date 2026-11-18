package store

import (
	"errors"
	"testing"
	"time"
)

// The task-fields pass on both dialects: the snapshot rule behind the
// task_fields consumer (a snapshot prepared once per sha, and again when the
// task changed), the fields gate, and the binding link the console reads
// (hk 5077, hk 4899 B4b). The cases use only the store's public methods, so
// PostgreSQL (TestYTAreaPostgres) and MySQL (TestMySQLYTArea) run the same
// fixtures.
func init() {
	// ytFieldsFixture creates a task in project "handoffkeep" (mapped to
	// YouTrack project 0-1) and, when bind is set, its verified binding.
	// The fields mode is left to the case: a binding published with the mode
	// on also prepares the first snapshot, so the cases that count a
	// preparation turn the mode on after binding.
	ytFieldsFixture := func(t *testing.T, s *Store, bind bool) (Task, YTTaskBinding) {
		t.Helper()
		ctx := t.Context()
		if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ProjectID: "0-1", ShortName: "HK"}); err != nil {
			t.Fatal(err)
		}
		task, err := s.CreateTask(ctx, Task{Lane: "yt-fields-case", Project: projectPtr("handoffkeep"), Title: "fields task",
			Kind: "implement", State: StateBacklog, CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		if !bind {
			return task, YTTaskBinding{}
		}
		b, err := s.AddYTTaskBinding(ctx, YTTaskBinding{TaskID: task.ID, Tenant: "yt.example", IssueID: "3-101",
			IssueReadable: "HK-101", ProjectID: "0-1", BoundBy: "tester", BoundAt: time.Now().UTC()},
			YTCanaryCandidate{TaskID: task.ID, Project: "handoffkeep", ShortName: "HK", ProjectID: "0-1"})
		if err != nil {
			t.Fatal(err)
		}
		return task, b
	}

	registerYTAreaCase("fields/snapshot-prepared-once-per-sha", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, b := ytFieldsFixture(t, s, true)
		if b.IssueID != "3-101" {
			t.Fatalf("binding = %+v", b)
		}
		s.SetYTFieldsMode(YTFieldsOn, "yt.example")

		// The first run prepares the task's snapshot; the second finds the
		// same sha and writes nothing.
		first, err := s.BackfillYTFieldsOps(ctx, YTFieldsBackfillOptions{TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if first.Total.Scanned != 1 || first.Total.Prepared != 1 || first.Total.Ops != 1 {
			t.Fatalf("first backfill = %+v", first.Total)
		}
		second, err := s.BackfillYTFieldsOps(ctx, YTFieldsBackfillOptions{TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if second.Total.Unchanged != 1 || second.Total.Prepared != 0 || second.Total.Ops != 0 {
			t.Fatalf("second backfill = %+v", second.Total)
		}

		// A task change (a new state) is a new snapshot sha: one more op.
		if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: StateDeveloping, By: "tester", NoJob: "fields case"}); err != nil {
			t.Fatal(err)
		}
		third, err := s.BackfillYTFieldsOps(ctx, YTFieldsBackfillOptions{TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if third.Total.Prepared != 1 || third.Total.Ops != 1 {
			t.Fatalf("after the change = %+v", third.Total)
		}
	})

	registerYTAreaCase("fields/unbound-and-mode-off", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _ := ytFieldsFixture(t, s, false)
		s.SetYTFieldsMode(YTFieldsOn, "yt.example")
		// A task with no binding is unbound: its issue_create publishes the
		// binding and prepares the first snapshot then.
		report, err := s.BackfillYTFieldsOps(ctx, YTFieldsBackfillOptions{TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if report.Total.Unbound != 1 || report.Total.Ops != 0 {
			t.Fatalf("unbound backfill = %+v", report.Total)
		}
		// Mode off admits nothing and the backfill refuses.
		s.SetYTFieldsMode(YTFieldsOff, "")
		if _, err := s.BackfillYTFieldsOps(ctx, YTFieldsBackfillOptions{TaskID: task.ID}); !errors.Is(err, ErrYTFieldsOff) {
			t.Fatalf("mode off err = %v, want ErrYTFieldsOff", err)
		}
	})

	registerYTAreaCase("fields/issue-task-ids-rank-binding-first", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _ := ytFieldsFixture(t, s, true)
		ids, err := s.YTIssueTaskIDs(ctx, "yt.example")
		if err != nil {
			t.Fatal(err)
		}
		got, ok := ids["3-101"]
		if !ok || got.TaskID != task.ID || got.Rank != YTLinkBinding {
			t.Fatalf("issue task = %+v found=%v (all %+v)", got, ok, ids)
		}
		// Another tenant's read does not see the binding.
		other, err := s.YTIssueTaskIDs(ctx, "other.example")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := other["3-101"]; ok {
			t.Fatalf("binding leaked into another tenant: %+v", other)
		}
	})
}

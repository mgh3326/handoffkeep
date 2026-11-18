package store

import (
	"testing"
	"time"
)

// The automatic binder and the binding lifecycle on both dialects: the
// cursor scan that makes an intent, the retry backoff that ends at an
// exclusion, and a binding's publication and unbind with its history action
// read back (hk 4899 B4b, hk 5077). The cases use only the store's public
// methods, so PostgreSQL (TestYTAreaPostgres) and MySQL (TestMySQLYTArea) run
// the same fixtures.
func init() {
	// ytBindingsTask files a task with no project: such a task agrees with
	// any YouTrack project that no youtrack_projects row maps (the import's
	// HK bucket), so a binding can be published against it without a project
	// mapping.
	ytBindingsTask := func(t *testing.T, s *Store, title string) Task {
		t.Helper()
		task, err := s.CreateTask(t.Context(), Task{Lane: "yt-bindings-case", Title: title, Kind: "implement", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	registerYTAreaCase("bindings/scan-advances-cursor-and-makes-intent", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task := ytBindingsTask(t, s, "binder candidate")
		if _, err := s.CreateTaskComment(ctx, task.ID, "tester", "a comment the binder sees"); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		scan, err := s.ScanYTBindingCandidates(ctx, "yt.example", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		if scan.Comments != 1 || scan.Tasks != 1 {
			t.Fatalf("first scan = %+v, want one comment and one intent", scan)
		}
		// The intent is pending for this tenant alone; it is due within the
		// hour, whether or not an issue for the task is still in flight.
		due, err := s.YTBindingIntentsDue(ctx, "yt.example", now.Add(2*time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].TaskID != task.ID || due[0].State != YTBindingIntentPending || due[0].Attempts != 0 {
			t.Fatalf("intents due = %+v, want the task's pending intent", due)
		}
		// The cursor moved with the intent, so the same comment is not read
		// again and no second row is made for the task.
		scan, err = s.ScanYTBindingCandidates(ctx, "yt.example", 100, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if scan.Comments != 0 || scan.Tasks != 0 {
			t.Fatalf("rescan = %+v, want nothing new", scan)
		}
		// Another tenant's read does not see the intent.
		other, err := s.YTBindingIntentsDue(ctx, "other.example", now, 10)
		if err != nil || len(other) != 0 {
			t.Fatalf("other tenant intents = %+v err=%v", other, err)
		}
		if err := s.DropYTBindingIntent(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		due, err = s.YTBindingIntentsDue(ctx, "yt.example", now.Add(time.Hour), 10)
		if err != nil || len(due) != 0 {
			t.Fatalf("after drop = %+v err=%v", due, err)
		}
	})

	registerYTAreaCase("bindings/retry-backoff-then-exclude", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task := ytBindingsTask(t, s, "binder refusal")
		if _, err := s.CreateTaskComment(ctx, task.ID, "tester", "a comment the binder sees"); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if _, err := s.ScanYTBindingCandidates(ctx, "yt.example", 100, now); err != nil {
			t.Fatal(err)
		}
		if err := s.RetryYTBindingIntent(ctx, task.ID, YTBindingIntentRefused, "no_issue", "the project has no issue", now); err != nil {
			t.Fatal(err)
		}
		// The first retry waits an hour: half an hour later the intent is not
		// due, two hours later it is, with its state and attempt count.
		due, err := s.YTBindingIntentsDue(ctx, "yt.example", now.Add(30*time.Minute), 10)
		if err != nil || len(due) != 0 {
			t.Fatalf("due inside the backoff = %+v err=%v", due, err)
		}
		due, err = s.YTBindingIntentsDue(ctx, "yt.example", now.Add(2*time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].State != YTBindingIntentRefused || due[0].Reason != "no_issue" || due[0].Attempts != 1 {
			t.Fatalf("due after the backoff = %+v", due)
		}
		// The operator unbound the task: the intent is terminal, and a late
		// retry does not bring it back.
		if err := s.ExcludeYTBindingIntent(ctx, task.ID, "operator_unbound", now); err != nil {
			t.Fatal(err)
		}
		if err := s.RetryYTBindingIntent(ctx, task.ID, YTBindingIntentError, "late", "late read", now); err != nil {
			t.Fatal(err)
		}
		due, err = s.YTBindingIntentsDue(ctx, "yt.example", now.Add(48*time.Hour), 10)
		if err != nil || len(due) != 0 {
			t.Fatalf("excluded intent due = %+v err=%v", due, err)
		}
	})

	registerYTAreaCase("bindings/bind-then-unbind-records-history", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task := ytBindingsTask(t, s, "bind and unbind")
		verified, err := s.YTCanaryCandidate(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.AddYTTaskBinding(ctx, YTTaskBinding{TaskID: task.ID, Tenant: "yt.example", IssueID: "3-201",
			IssueReadable: "HK-201", ProjectID: "0-0", BoundBy: "tester"}, verified)
		if err != nil {
			t.Fatal(err)
		}
		if b.IssueID != "3-201" || b.Origin != YTBindingImportVerified || b.CommentsFrom != 0 {
			t.Fatalf("stored binding = %+v", b)
		}
		if excluded, err := s.YTBindingExcluded(ctx, task.ID); err != nil || excluded {
			t.Fatalf("YTBindingExcluded after bind = %v err=%v", excluded, err)
		}
		// Publishing the same binding again keeps the stored row.
		again, err := s.AddYTTaskBinding(ctx, b, verified)
		if err != nil || again.IssueID != b.IssueID {
			t.Fatalf("repeat bind = %+v err=%v", again, err)
		}
		res, err := s.UnbindYTTaskBinding(ctx, task.ID, YTUnbindOptions{By: "tester", Reason: "wrong issue"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Removed || res.Binding == nil || res.Binding.IssueID != "3-201" {
			t.Fatalf("unbind = %+v", res)
		}
		// The history row is what marks the task excluded: the latest action
		// is now 'unbound'.
		if excluded, err := s.YTBindingExcluded(ctx, task.ID); err != nil || !excluded {
			t.Fatalf("YTBindingExcluded after unbind = %v err=%v (history read back)", excluded, err)
		}
		if got, found, err := s.YTTaskBinding(ctx, task.ID); err != nil || found {
			t.Fatalf("binding after unbind = %+v found=%v err=%v", got, found, err)
		}
		// A second unbind has nothing to remove and writes nothing.
		againRes, err := s.UnbindYTTaskBinding(ctx, task.ID, YTUnbindOptions{By: "tester", Reason: "again"})
		if err != nil || againRes.Removed {
			t.Fatalf("second unbind = %+v err=%v", againRes, err)
		}
	})
}

package store

import (
	"strconv"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The link pass's two MySQL argument orders the review found: the desired
// set's (tenant, task) and the wake insert's transaction token. A binding
// published for a task with a child wakes that child through sync_log, and a
// later change of the child prepares its parent-edge snapshot through
// ytLinksDesiredTx. The case makes no dialect-specific SQL.
func init() {
	registerYTAreaCase("links/binding-wakes-writers-and-feed-prepares", func(t *testing.T, s *Store) {
		ctx := t.Context()
		if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ProjectID: "0-30", ShortName: "HK"}); err != nil {
			t.Fatal(err)
		}
		parent, err := s.CreateTask(ctx, Task{Lane: "yt-links-wake", Project: projectPtr("handoffkeep"), Title: "parent",
			Kind: "implement", State: StateBacklog, CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		child, err := s.CreateTask(ctx, Task{Lane: "yt-links-edge", Project: projectPtr("handoffkeep"), Title: "child",
			Kind: "implement", State: StateBacklog, ParentID: &parent.ID, CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		bind := func(task Task, issue, readable string) {
			t.Helper()
			cand, err := s.YTCanaryCandidate(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddYTTaskBinding(ctx, YTTaskBinding{TaskID: task.ID, Tenant: "yt.example", IssueID: issue,
				IssueReadable: readable, ProjectID: "0-30", BoundBy: "tester"}, cand); err != nil {
				t.Fatal(err)
			}
		}
		// The parent's publication wakes its child (a writer pointing at it)
		// with one sync_log row per writer.
		bind(parent, "3-10", "HK-10")
		bind(child, "3-20", "HK-20")

		// A first pass creates the consumers' cursors; history before a cursor
		// is the backfill's, so the child's change comes after it.
		if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
			t.Fatal(err)
		}
		s.SetYTLinksMode(YTLinksOn, "yt.example")
		if _, err := s.TransitionTaskWith(ctx, child.ID, TransitionInput{To: StateDeveloping, By: "tester", NoJob: "links case"}); err != nil {
			t.Fatal(err)
		}
		passes, err := s.RunYTFeedConsumers(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		prepared := false
		for _, pass := range passes {
			if pass.Consumer == ytLinksConsumer && pass.Handled == 0 {
				t.Fatalf("links pass handled nothing: %+v", pass)
			}
		}
		ops, err := s.YTWriteLedger().ListState(ctx, ytwrite.StatePrepared, YTFeedPassMax)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			if op.Kind == ytwrite.KindIssueLinks && op.EntityID == strconv.FormatInt(child.ID, 10) {
				prepared = true
			}
		}
		if !prepared {
			t.Fatalf("the child's parent edge prepared no issue_links op: %+v", ops)
		}
	})
}

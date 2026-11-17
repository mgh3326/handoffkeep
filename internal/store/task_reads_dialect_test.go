package store

import (
	"errors"
	"testing"
	"time"
)

// Dialect-neutral cases of the task read methods (hk 5074, A3).

func init() {
	registerTaskAreaCase("reads/briefs-and-batches", func(t *testing.T, s *Store) {
		ctx := t.Context()
		a, _, err := s.IntakeTask(ctx, IntakeInput{Source: "reads-test", SourceRef: "batch/a", Title: "Batch a", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := s.IntakeTask(ctx, IntakeInput{Source: "reads-test", SourceRef: "batch/b", Title: "Batch b", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		missing := int64(999999999)

		briefs, err := s.TaskBriefs(ctx, []int64{b.ID, missing, a.ID})
		if err != nil || len(briefs) != 2 {
			t.Fatalf("TaskBriefs = %+v err=%v, want the two existing tasks", briefs, err)
		}
		if got := briefs[a.ID]; got.ID != a.ID || got.Title != "Batch a" || got.State != StateTriage || got.Category != TaskCategory(StateTriage) {
			t.Fatalf("brief of a = %+v", got)
		}
		if empty, err := s.TaskBriefs(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("TaskBriefs(nil) = %+v err=%v", empty, err)
		}
		tooMany := make([]int64, 1001)
		if _, err := s.TaskBriefs(ctx, tooMany); err == nil {
			t.Fatal("TaskBriefs accepted 1001 ids")
		}

		tasks, err := s.TasksByIDs(ctx, []int64{b.ID, missing, a.ID})
		if err != nil || len(tasks) != 2 || tasks[0].ID != a.ID || tasks[1].ID != b.ID {
			t.Fatalf("TasksByIDs = %+v err=%v, want a then b in id order", tasks, err)
		}
		if tasks[0].Source != "reads-test" || tasks[0].SourceRef != "batch/a" || tasks[0].State != StateTriage {
			t.Fatalf("TasksByIDs row = %+v", tasks[0])
		}
		if none, err := s.TasksByIDs(ctx, nil); err != nil || none == nil || len(none) != 0 {
			t.Fatalf("TasksByIDs(nil) = %+v err=%v", none, err)
		}
		if _, err := s.TasksByIDs(ctx, tooMany); err == nil {
			t.Fatal("TasksByIDs accepted 1001 ids")
		}
	})

	registerTaskAreaCase("reads/task-events", func(t *testing.T, s *Store) {
		ctx := t.Context()
		project := "reads-events"
		_, err := s.AddTaskProject(ctx, project, "tester")
		needPorted(t, s, "AddTaskProject", err)
		x, err := s.CreateTask(ctx, Task{Lane: "reads-events", Title: "Task with events", Kind: "implement", CreatedBy: "tester", Project: &project})
		needPorted(t, s, "CreateTask", err)
		for _, to := range []string{StateUnstarted, StateBacklog} {
			_, err = s.TransitionTask(ctx, x.ID, to, "tester", "move to "+to, nil, "")
			needPorted(t, s, "TransitionTask", err)
		}

		all, err := s.ListTaskEvents(ctx, x.ID, 0, TaskEventListMax)
		if err != nil || len(all) < 2 {
			t.Fatalf("ListTaskEvents = %+v err=%v, want at least the two transitions", all, err)
		}
		for i, e := range all {
			if e.TaskID != x.ID || e.At.IsZero() || (i > 0 && e.ID <= all[i-1].ID) {
				t.Fatalf("event %d = %+v, want this task's events in id order", i, e)
			}
		}
		last := all[len(all)-1]
		if last.From != StateUnstarted || last.To != StateBacklog || last.By != "tester" {
			t.Fatalf("last event = %+v, want the move unstarted to backlog by tester", last)
		}
		// Paging by the id cursor visits each event once.
		var paged []int64
		var after int64
		for {
			page, err := s.ListTaskEvents(ctx, x.ID, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			paged = append(paged, page[0].ID)
			after = page[0].ID
		}
		if len(paged) != len(all) {
			t.Fatalf("paged %v, want the %d events of the full read", paged, len(all))
		}
		if past, err := s.ListTaskEvents(ctx, x.ID, last.ID, 10); err != nil || past == nil || len(past) != 0 {
			t.Fatalf("read past the last event = %+v err=%v, want an empty list", past, err)
		}
		if _, err := s.ListTaskEvents(ctx, 999999999, 0, 10); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("missing task err = %v, want ErrTaskNotFound", err)
		}
		for _, bad := range [][3]int64{{0, 0, 10}, {x.ID, -1, 10}, {x.ID, 0, 0}, {x.ID, 0, TaskEventListMax + 1}} {
			if _, err := s.ListTaskEvents(ctx, bad[0], bad[1], int(bad[2])); err == nil {
				t.Fatalf("ListTaskEvents%v accepted", bad)
			}
		}
	})

	registerTaskAreaCase("reads/comments", func(t *testing.T, s *Store) {
		ctx := t.Context()
		project := "reads-comments"
		_, err := s.AddTaskProject(ctx, project, "tester")
		needPorted(t, s, "AddTaskProject", err)
		x, err := s.CreateTask(ctx, Task{Lane: "reads-comments", Title: "Task with comments", Kind: "implement", CreatedBy: "tester", Project: &project})
		needPorted(t, s, "CreateTask", err)
		var ids []int64
		for _, body := range []string{"first", "second", "third"} {
			c, err := s.CreateTaskComment(ctx, x.ID, "tester", body)
			needPorted(t, s, "CreateTaskComment", err)
			ids = append(ids, c.ID)
		}

		tail, err := s.RecentTaskComments(ctx, x.ID, 2)
		if err != nil || len(tail) != 2 || tail[0].Body != "second" || tail[1].Body != "third" {
			t.Fatalf("RecentTaskComments(2) = %+v err=%v, want second then third", tail, err)
		}
		whole, err := s.RecentTaskComments(ctx, x.ID, TaskCommentListMax)
		if err != nil || len(whole) != 3 || whole[0].Body != "first" {
			t.Fatalf("RecentTaskComments(max) = %+v err=%v", whole, err)
		}
		if none, err := s.RecentTaskComments(ctx, 999999999, 5); err != nil || none == nil || len(none) != 0 {
			t.Fatalf("comments of a missing task = %+v err=%v, want an empty list", none, err)
		}
		for _, bad := range [][2]int64{{0, 5}, {x.ID, 0}, {x.ID, TaskCommentListMax + 1}} {
			if _, err := s.RecentTaskComments(ctx, bad[0], int(bad[1])); !errors.Is(err, ErrTaskCommentInvalid) {
				t.Fatalf("RecentTaskComments%v err = %v, want ErrTaskCommentInvalid", bad, err)
			}
		}

		owners, err := s.TaskIDsForComments(ctx, append([]int64{999999999}, ids...))
		if err != nil || len(owners) != 3 {
			t.Fatalf("TaskIDsForComments = %+v err=%v, want the three comments", owners, err)
		}
		for _, id := range ids {
			if owners[id] != x.ID {
				t.Fatalf("comment %d owner = %d, want %d", id, owners[id], x.ID)
			}
		}
		if empty, err := s.TaskIDsForComments(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("TaskIDsForComments(nil) = %+v err=%v", empty, err)
		}
		if _, err := s.TaskIDsForComments(ctx, make([]int64, 1001)); err == nil {
			t.Fatal("TaskIDsForComments accepted 1001 ids")
		}
	})

	registerTaskAreaCase("reads/sync-id-at", func(t *testing.T, s *Store) {
		ctx := t.Context()
		if id, err := s.SyncIDAt(ctx, time.Unix(0, 0)); err != nil || id != 0 {
			t.Fatalf("SyncIDAt(epoch) = %d err=%v, want 0", id, err)
		}
		future := time.Now().UTC().Add(time.Hour)
		before, err := s.SyncIDAt(ctx, future)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := s.IntakeTask(ctx, IntakeInput{Source: "reads-test", SourceRef: "sync/1", Title: "Sync position", CreatedBy: "tester"}); err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		after, err := s.SyncIDAt(ctx, future)
		if err != nil || after <= before {
			t.Fatalf("SyncIDAt after a committed write = %d (before %d) err=%v, want it to advance", after, before, err)
		}
	})
}

package store

import (
	"errors"
	"testing"
	"time"
)

// The issue-create lane's store surface runs on both dialects: the project
// mapping it reads (youtrack_projects), the task-origin mirror row it writes
// (task_origins), and the comment audit's read of a task's comments (hk 5077).
func init() {
	registerYTAreaCase("issues/project-mapping-round-trip", func(t *testing.T, s *Store) {
		ctx := t.Context()
		const hk = "yt-issues-case"
		p := YouTrackProject{HKProject: hk, ProjectID: "0-7", ShortName: "CASE"}
		if err := s.PutYouTrackProject(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, found, err := s.YouTrackProjectFor(ctx, hk)
		if err != nil || !found || got != p {
			t.Fatalf("read back %+v found=%v err=%v, want %+v", got, found, err, p)
		}
		// The resolved id is not a mapping write.
		if err := s.SetYouTrackProjectID(ctx, hk, "0-8"); err != nil {
			t.Fatal(err)
		}
		got, found, err = s.YouTrackProjectFor(ctx, hk)
		if err != nil || !found || got.ProjectID != "0-8" || got.ShortName != "CASE" {
			t.Fatalf("after id: %+v found=%v err=%v", got, found, err)
		}
		// A mapping write updates the row instead of adding one.
		p.ProjectID = "0-9"
		if err := s.PutYouTrackProject(ctx, p); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListYouTrackProjects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range list {
			if x.HKProject == hk {
				n++
				if x != p {
					t.Fatalf("listed %+v, want %+v", x, p)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d rows for %q, want 1", n, hk)
		}
	})

	registerYTAreaCase("issues/mirror-write-then-read", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-issues-case", SourceRef: "mirror", Title: "mirror task", Project: projectPtr("handoffkeep"), CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		in := YouTrackMirrorInput{TaskID: task.ID, IssueID: "3-11", DisplayRef: "HK-11",
			URL: "https://yt.example/issue/HK-11", Tenant: "yt.example", Role: "origin"}
		m, err := s.UpsertYouTrackMirror(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		// No push happened yet, so there is no watermark.
		if m.TaskID != task.ID || m.IssueID != "3-11" || m.DisplayRef != "HK-11" || m.URL != in.URL ||
			m.Tenant != "yt.example" || m.Role != "origin" || m.SyncedAt != nil {
			t.Fatalf("upserted %+v", m)
		}
		got, found, err := s.GetYouTrackMirror(ctx, task.ID)
		if err != nil || !found || got.IssueID != "3-11" {
			t.Fatalf("read back %+v found=%v err=%v", got, found, err)
		}
		// A second upsert updates the task's row in place and records the
		// watermark.
		at := time.Now().UTC().Truncate(time.Microsecond)
		in.IssueID, in.DisplayRef, in.Role, in.SyncedAt = "3-12", "HK-12", "mirror", at
		if _, err := s.UpsertYouTrackMirror(ctx, in); err != nil {
			t.Fatal(err)
		}
		got, found, err = s.GetYouTrackMirror(ctx, task.ID)
		if err != nil || !found || got.IssueID != "3-12" || got.DisplayRef != "HK-12" || got.Role != "mirror" {
			t.Fatalf("after update %+v found=%v err=%v", got, found, err)
		}
		if got.SyncedAt == nil || !got.SyncedAt.UTC().Equal(at) {
			t.Fatalf("synced_at %v, want %v", got.SyncedAt, at)
		}
		byIssue, found, err := s.FindYouTrackMirrorByIssueID(ctx, "3-12")
		if err != nil || !found || byIssue.TaskID != task.ID {
			t.Fatalf("by issue %+v found=%v err=%v", byIssue, found, err)
		}
		list, err := s.ListYouTrackMirrors(ctx, []int64{task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if one, ok := list[task.ID]; !ok || one.IssueID != "3-12" || len(list) != 1 {
			t.Fatalf("list %+v", list)
		}
	})

	registerYTAreaCase("issues/mirror-conflict-guard", func(t *testing.T, s *Store) {
		ctx := t.Context()
		first, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-issues-case", SourceRef: "conflict-a", Title: "a", Project: projectPtr("handoffkeep"), CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-issues-case", SourceRef: "conflict-b", Title: "b", Project: projectPtr("handoffkeep"), CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: first.ID, IssueID: "3-21",
			Tenant: "yt.example", Role: "origin"}); err != nil {
			t.Fatal(err)
		}
		// The issue already belongs to another task.
		_, err = s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: second.ID, IssueID: "3-21",
			Tenant: "yt.example", Role: "origin"})
		if !errors.Is(err, ErrYouTrackMirrorConflict) {
			t.Fatalf("second task: err=%v, want ErrYouTrackMirrorConflict", err)
		}
		// Nor may a bad role reach the table.
		_, err = s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: second.ID, IssueID: "3-22",
			Tenant: "yt.example", Role: "sideways"})
		if !errors.Is(err, ErrYouTrackMirrorConflict) {
			t.Fatalf("bad role: err=%v, want ErrYouTrackMirrorConflict", err)
		}
		if _, found, err := s.GetYouTrackMirror(ctx, second.ID); err != nil || found {
			t.Fatalf("second task row: found=%v err=%v, want none", found, err)
		}
		kept, found, err := s.GetYouTrackMirror(ctx, first.ID)
		if err != nil || !found || kept.IssueID != "3-21" {
			t.Fatalf("first task row %+v found=%v err=%v", kept, found, err)
		}
	})

	registerYTAreaCase("issues/audit-of-empty-task", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-issues-case", SourceRef: "audit", Title: "audit task", Project: projectPtr("handoffkeep"), CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		// A task with no comments is an empty audit, not an error.
		got, err := s.YTAuditTaskComments(ctx, task.ID, "3-31")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Comments) != 0 || len(got.Links) != 0 || len(got.PulledYTIDs) != 0 {
			t.Fatalf("audit of an empty task: %+v", got)
		}
		cands, err := s.YTAuditTasks(ctx, "", task.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) != 1 || cands[0].TaskID != task.ID {
			t.Fatalf("audit tasks %+v", cands)
		}
	})
}

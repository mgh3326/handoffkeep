package store

import (
	"slices"
	"testing"
	"time"
)

// hk 4121: the restore drill's PG reads. Bound tasks are listed by tenant and
// project; the comment sample is the top tasks by comment count plus a
// seeded random rest, the same for one seed and different for another; a
// task's comments carry their op and legacy links, and the pull's are left
// out with their YouTrack ids.
func TestYTRebuildReads(t *testing.T) {
	s := schemaStore(t, "yt_rebuild")
	ctx := t.Context()
	ytbMapped(t, s)
	bind := func(task int64, tenant string) {
		ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at)
			VALUES ($1,$2,$3,$4,'test',now())`, task, tenant, ytfIssue(task), ytbProjectID)
	}
	at := time.Date(2026, 11, 1, 2, 3, 4, 567891000, time.UTC)
	comment := func(task int64, author, body string) int64 {
		var id int64
		if err := s.pool.QueryRow(ctx, `INSERT INTO task_comments(task_id, body, author, created_at) VALUES ($1,$2,$3,$4) RETURNING id`,
			task, body, author, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var bound []int64
	for i := range 40 {
		task := ytcTask(t, s, ytbProject)
		bind(task, ytcTenant)
		bound = append(bound, task)
		// Task i has i comments, and one from the pull that never counts.
		for range i {
			comment(task, "worker:x", "c")
		}
		comment(task, YouTrackOwnedActor, "pulled")
	}
	other := ytcTask(t, s, "other-project")
	bind(other, ytcTenant)
	elsewhere := ytcTask(t, s, ytbProject)
	bind(elsewhere, "other.youtrack.cloud")
	unbound := ytcTask(t, s, ytbProject)

	// Bound tasks, paged, on the tenant only; --project narrows them.
	var listed []int64
	for after := int64(0); ; {
		page, err := s.YTRebuildTasks(ctx, ytcTenant, "", after, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			listed = append(listed, x.TaskID)
			after = x.TaskID
			if x.IssueID != ytfIssue(x.TaskID) || x.Title != "ytc task" {
				t.Fatalf("task %+v", x)
			}
		}
		if len(page) < 7 {
			break
		}
	}
	if want := append(slices.Clone(bound), other); !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want %v (not %d on another tenant, not unbound %d)", listed, want, elsewhere, unbound)
	}
	only, err := s.YTRebuildTasks(ctx, ytcTenant, "other-project", 0, 10)
	if err != nil || len(only) != 1 || only[0].TaskID != other || only[0].Project != "other-project" {
		t.Fatalf("--project: %+v %v", only, err)
	}

	// The sample: the 5 tasks with most comments, and 10 of the rest by seed.
	sample := func(seed int64) (top, rnd []int64) {
		t.Helper()
		got, err := s.YTRebuildSampleTasks(ctx, ytcTenant, ytbProject, seed, 10, 5)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, x := range got {
			ids = append(ids, x.TaskID)
			if x.Top {
				top = append(top, x.TaskID)
			} else {
				rnd = append(rnd, x.TaskID)
			}
		}
		if !slices.IsSorted(ids) {
			t.Fatalf("sample not in id order: %v", ids)
		}
		return top, rnd
	}
	top1, rnd1 := sample(42)
	if !slices.Equal(top1, bound[35:]) || len(rnd1) != 10 {
		t.Fatalf("top %v (want %v), random %v", top1, bound[35:], rnd1)
	}
	for _, id := range rnd1 {
		if slices.Contains(top1, id) || !slices.Contains(bound, id) {
			t.Fatalf("random pick %d is a top task or outside the project", id)
		}
	}
	top2, rnd2 := sample(42)
	if !slices.Equal(top1, top2) || !slices.Equal(rnd1, rnd2) {
		t.Fatalf("seed 42 picked %v then %v", rnd1, rnd2)
	}
	_, rnd3 := sample(43)
	if slices.Equal(rnd1, rnd3) {
		t.Fatalf("seeds 42 and 43 picked the same tasks %v", rnd1)
	}

	// One task's comments: the op link (an op of the bound issue) and the
	// legacy link; the pull's comment left out, its YouTrack id kept.
	task := bound[3]
	withOp := comment(task, "worker:op", "sent by the protocol")
	status, op, err := s.EnqueueYTAuditComment(ctx, withOp)
	if err != nil || status != YTAuditPrepared {
		t.Fatalf("enqueue: %q %v", status, err)
	}
	legacy := comment(task, "worker:old", "mirrored")
	pulled := comment(task, YouTrackOwnedActor, "from YouTrack")
	for _, l := range []struct {
		id   int64
		ytID string
	}{{legacy, "4-900"}, {pulled, "4-901"}} {
		ytFeedExec(t, s, `INSERT INTO youtrack_comment_links(hk_comment_id, task_id, youtrack_comment_id, created_at) VALUES ($1,$2,$3,now())`, l.id, task, l.ytID)
	}
	got, err := s.YTRebuildTaskComments(ctx, task, ytfIssue(task))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 5 || !got.PulledYTIDs["4-901"] || len(got.PulledYTIDs) != 1 {
		t.Fatalf("comments %+v pulled %v", got.Comments, got.PulledYTIDs)
	}
	byID := map[int64]YTRebuildComment{}
	for _, c := range got.Comments {
		byID[c.ID] = c
	}
	if c := byID[withOp]; c.OpID != op.ID || c.Author != "worker:op" || !c.CreatedAt.Equal(at) || c.CreatedAt.Location() != time.UTC {
		t.Fatalf("op comment %+v, op %d", c, op.ID)
	}
	if c := byID[legacy]; c.LegacyYTID != "4-900" || c.OpID != 0 {
		t.Fatalf("legacy comment %+v", c)
	}
	// Against another issue the op is not this issue's.
	got, err = s.YTRebuildTaskComments(ctx, task, "2-other")
	if err != nil || got.Comments[3].ID != withOp || got.Comments[3].OpID != 0 {
		t.Fatalf("other issue: %+v %v", got.Comments, err)
	}
}

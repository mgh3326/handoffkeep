package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ytaNoProjectTask inserts a task with no project (NULL), as a dots request
// or an old task has, created an hour ago, and returns its id.
func ytaNoProjectTask(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	now := time.Now().UTC().Add(-time.Hour)
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at)
		VALUES('yta-lane','yta task','implement','backlog','test',$1,$1,$1) RETURNING id`, now).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ytaApplyAdopted dispatches op and applies it as an adoption of issue.
func ytaApplyAdopted(t *testing.T, s *Store, op ytwrite.Op, issue string) {
	t.Helper()
	ytiDispatch(t, s, op)
	maps := ytiMaps(op, issue)
	maps[0].Adopted = true
	if got, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, issue, maps); err != nil || got.Reason != "" {
		t.Fatalf("apply %+v %v", got, err)
	}
}

// hk 4301: an issue_create op that adopts an existing issue starts the
// binding's comment catch-up at the comments the task had when the op was
// prepared, not at its settlement: a comment written while the op was
// pending got no op (no binding yet) and the adopted issue cannot hold it,
// so the catch-up must still copy it. An op with no recorded boundary (one a
// previous release prepared) keeps the settlement-time cutoff.
func TestYTIssueAdoptedBindingStartsAtThePrepareBoundary(t *testing.T) {
	s := schemaStore(t, "yta_bound")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	before := ytcuRawComment(t, s, task, "worker").ID
	r, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, ytbProject, false)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].Prepared != 1 {
		t.Fatalf("backfill %+v %v", r, err)
	}
	op := ytiOps(t, s, task)[0]
	pending := ytcuRawComment(t, s, task, "worker").ID
	ytaApplyAdopted(t, s, op, "2-adopted")
	b, found, err := s.YTTaskBinding(t.Context(), task)
	if err != nil || !found || b.CommentsFrom != before || b.Origin != YTBindingOpApplied {
		t.Fatalf("binding %+v %v %v, want comments_from %d (before the op), not %d (written while it was pending)", b, found, err, before, pending)
	}

	// No boundary row: the cutoff is the task's comments at the apply.
	old := ytcTask(t, s, ytbProject)
	ytcuRawComment(t, s, old, "worker")
	oldOp := ytiPrepare(t, s, old, ytcTenant)
	last := ytcuRawComment(t, s, old, "worker").ID
	ytaApplyAdopted(t, s, oldOp, "2-adopted-old")
	if b, found, err := s.YTTaskBinding(t.Context(), old); err != nil || !found || b.CommentsFrom != last {
		t.Fatalf("binding %+v %v %v, want comments_from %d", b, found, err, last)
	}

	// A created (not adopted) issue still starts at 0.
	fresh := ytcTask(t, s, ytbProject)
	ytcuRawComment(t, s, fresh, "worker")
	if _, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, ytbProject, false); err != nil {
		t.Fatal(err)
	}
	freshOp := ytiOps(t, s, fresh)[0]
	ytiDispatch(t, s, freshOp)
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), freshOp.ID, "2-created", ytiMaps(freshOp, "2-created")); err != nil {
		t.Fatal(err)
	}
	if b, found, err := s.YTTaskBinding(t.Context(), fresh); err != nil || !found || b.CommentsFrom != 0 {
		t.Fatalf("created binding %+v %v %v, want comments_from 0", b, found, err)
	}
}

// hk 4336 (1): a no-project task's op, prepared for the HK bucket, takes the
// task's project when the project is set and mapped before the op is sent
// (claimYTIssueTargetTx, hk 4318). (2): the op a project set prepares for a
// task that had none carries the time of that change, not the task's
// creation time.
func TestYTIssueBucketOpTakesTheProjectSetLater(t *testing.T) {
	s := schemaStore(t, "yta_bucket")
	ytbMapped(t, s)
	ytrProject(t, s, "yta-later", "0-9")
	s.SetYTIssuesBucketProject("0-hk")
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	op := ytiOps(t, s, task.ID)[0]
	if op.YTTargetID != "0-hk" {
		t.Fatalf("bucket op %+v", op)
	}
	ytrMove(t, s, task.ID, "yta-later")
	ytiDispatch(t, s, op)
	if got := ytrOp(t, s, op.ID); got.YTTargetID != "0-9" || ytrProjectOf(t, got) != "yta-later" || got.ID != op.ID {
		t.Fatalf("dispatched op %+v, want target 0-9 for yta-later", got)
	}

	s.SetYTIssuesMode(YTIssuesOff, "")
	later, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Later", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	updated, changed, err := s.SetTaskProject(t.Context(), later.ID, "yta-later", "test", "assign")
	if err != nil || !changed {
		t.Fatalf("SetTaskProject changed=%t err=%v", changed, err)
	}
	ops := ytiOps(t, s, later.ID)
	if len(ops) != 1 || !ops[0].CreatedAt.Equal(updated.UpdatedAt) || ops[0].CreatedAt.Equal(later.CreatedAt) {
		t.Fatalf("ops %+v, want created_at %s (the project set), task created %s", ops, updated.UpdatedAt, later.CreatedAt)
	}
}

// hk 4336 (PR #350 comment 4552667543): issues-backfill without --project
// prepares a no-project task's op in the HK bucket when the bucket id is
// known (the CLI resolves it before the backfill), and counts the task
// unmapped when it is not.
func TestYTIssueBackfillPutsNoProjectTasksInTheBucket(t *testing.T) {
	s := schemaStore(t, "yta_backfill")
	task := ytaNoProjectTask(t, s)
	r, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, "", false)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].Project != "" || r.Projects[0].Unmapped != 1 {
		t.Fatalf("backfill without the bucket %+v %v", r, err)
	}
	if ops := ytiOps(t, s, task); len(ops) != 0 {
		t.Fatalf("ops %+v", ops)
	}
	s.SetYTIssuesBucketProject("0-hk")
	r, err = s.BackfillYTIssueOps(t.Context(), ytcTenant, "", false)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].Prepared != 1 {
		t.Fatalf("backfill with the bucket %+v %v", r, err)
	}
	ops := ytiOps(t, s, task)
	if len(ops) != 1 || ops[0].YTTargetID != "0-hk" || ops[0].Tenant != ytcTenant || ytrProjectOf(t, ops[0]) != "" {
		t.Fatalf("ops %+v", ops)
	}
}

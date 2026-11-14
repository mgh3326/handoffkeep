package store

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// hk 4569 (redesign of hk 4457): a comment queued behind its task's
// issue_create whose binding disagrees at the claim is rejected there, as
// before, so nothing on the lane waits for a later op; once the binding
// agrees, the worker's requeue pass copies it again as a new op at the tail
// of the lane.

const (
	ytrqOther   = "ytrq-other"
	ytrqOtherID = "0-9"
	ytrqThird   = "ytrq-third"
	ytrqThirdID = "0-8"
	ytrqIssue   = "2-900"
)

// ytrqMapped adds two more mapped projects to a ytqSetup store.
func ytrqMapped(t *testing.T, s *Store) {
	t.Helper()
	for _, p := range []YouTrackProject{{HKProject: ytrqOther, ProjectID: ytrqOtherID, ShortName: "HKO"}, {HKProject: ytrqThird, ProjectID: ytrqThirdID, ShortName: "HKT"}} {
		if _, err := s.AddTaskProject(t.Context(), p.HKProject, "test"); err != nil {
			t.Fatal(err)
		}
		if err := s.PutYouTrackProject(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
}

// ytrqMoveTask sets the task's hk project.
func ytrqMoveTask(t *testing.T, s *Store, task int64, project string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET project=$2 WHERE id=$1`, task, project); err != nil {
		t.Fatal(err)
	}
}

// ytrqQueued writes a comment on task and returns its queued (targetless) op.
func ytrqQueued(t *testing.T, s *Store, task int64, body string) (TaskComment, ytwrite.Op) {
	t.Helper()
	c, err := s.CreateTaskComment(t.Context(), task, "worker", body)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.YTTargetID != "" || op.State != ytwrite.StatePrepared {
		t.Fatalf("queued op %+v %v", op, ok)
	}
	return c, op
}

// ytrqApplyCreate applies the dispatched create with ytrqIssue as its issue.
func ytrqApplyCreate(t *testing.T, s *Store, create ytwrite.Op) {
	t.Helper()
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), create.ID, ytrqIssue, ytiMaps(create, ytrqIssue)); err != nil {
		t.Fatal(err)
	}
}

// ytrqPrepareMove prepares the task's move to projectID, as the fields feed
// does (prepareYTOnceTx), and returns it.
func ytrqPrepareMove(t *testing.T, s *Store, task int64, projectID string) ytwrite.Op {
	t.Helper()
	n, err := ytwrite.MoveOp(ytwrite.MoveRequest{Principal: YTFieldsPrincipal, Actor: YTFieldsPrincipal, Tenant: ytcTenant,
		IssueID: ytrqIssue, ProjectID: projectID, TaskID: task})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	mv, made, err := prepareYTOnceTx(t.Context(), tx, time.Now().UTC(), n, n.PayloadSHA)
	if err != nil || !made {
		t.Fatalf("prepare move: %v made=%v", err, made)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return mv
}

// ytrqRejectedAmbiguous claims the queued op and checks the claim rejected
// it ambiguous_issue without sending anything.
func ytrqRejectedAmbiguous(t *testing.T, s *Store, op ytwrite.Op) {
	t.Helper()
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimNotPrepared {
		t.Fatalf("claim of the queued comment = %s", out)
	}
	got, err := s.YTWriteLedger().ByID(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ytwrite.StateRejected || got.Reason != ytwrite.ReasonAmbiguousIssue || got.YTTargetID != "" || got.Attempts != 0 {
		t.Fatalf("queued comment after its claim %+v", got)
	}
}

// ytrqRequeue runs one requeue pass for the test tenant.
func ytrqRequeue(t *testing.T, s *Store) []YTRequeued {
	t.Helper()
	got, err := s.RequeueYTMovedComments(t.Context(), ytcTenant, 100)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ytrqWantRequeued checks one pass result: comment c's rejected op from was
// replaced by a prepared op that targets the issue, comes after every op in
// after, and now owns the comment's link (the projection reads it).
func ytrqWantRequeued(t *testing.T, s *Store, got []YTRequeued, c TaskComment, from ytwrite.Op, after ...ytwrite.Op) ytwrite.Op {
	t.Helper()
	if len(got) != 1 || got[0].CommentID != c.ID || got[0].From != from.ID {
		t.Fatalf("requeued %+v, want comment %d from op %d", got, c.ID, from.ID)
	}
	op := got[0].Op
	if op.State != ytwrite.StatePrepared || op.YTTargetID != ytrqIssue || op.Tenant != ytcTenant || op.Key != ytRequeueKey(c.ID, from.ID) ||
		op.Kind != ytwrite.KindCommentAppend || op.Principal != YTCommentPrincipal || op.EntityID != from.EntityID || op.Actor != from.Actor {
		t.Fatalf("requeued op %+v", op)
	}
	for _, a := range after {
		if op.ID <= a.ID {
			t.Fatalf("requeued op %d is not after op %d (%s)", op.ID, a.ID, a.Kind)
		}
	}
	if linked, ok := ytcOpFor(t, s, c.ID); !ok || linked.ID != op.ID {
		t.Fatalf("comment %d is linked to %+v %v, want op %d", c.ID, linked, ok, op.ID)
	}
	if prev, err := s.YTWriteLedger().ByID(t.Context(), from.ID); err != nil || prev.State != ytwrite.StateRejected || prev.Reason != ytwrite.ReasonAmbiguousIssue {
		t.Fatalf("the rejected op changed: %+v %v", prev, err)
	}
	return op
}

// ytrqDeliver drives the comment worker over fake until op resolves and
// checks it was sent once, to the issue, as the issue's last comment. One
// fake per test: two fakes would hand out the same YouTrack comment ids.
func ytrqDeliver(t *testing.T, s *Store, fake *ytwritetest.FakeYouTrack, op ytwrite.Op, body string) {
	t.Helper()
	before := len(fake.Comments(ytrqIssue))
	counting := &ytcCounting{Remote: fake}
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, counting, &offset), op.ID)
	if got.State != ytwrite.StateApplied || got.YTTargetID != ytrqIssue || counting.posts.Load() != 1 {
		t.Fatalf("requeued op after the drive %+v, posts %d", got, counting.posts.Load())
	}
	if cs := fake.Comments(ytrqIssue); len(cs) != before+1 || !contains(cs[len(cs)-1].Text, body) {
		t.Fatalf("remote comments %+v", cs)
	}
}

// The review's sequence (PR #383, r4564745703) with the move driven through
// the ledger: C1 queued behind the create, C2 written once the binding exists,
// then the task moves and a move M is prepared behind both. C1 is rejected at
// its claim, C2 goes out, M applies; only then is C1 copied again, after M.
// While the move is pending, the pass copies nothing.
func TestYTCommentRequeuedAfterTheMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_move")
	ytrqMapped(t, s)
	ctx := t.Context()
	logs := ytbLog(t)
	c1, op1 := ytrqQueued(t, s, x.ID, "queued behind the create")
	ytiDispatch(t, s, create)
	ytrqApplyCreate(t, s, create)
	c2, err := s.CreateTaskComment(ctx, x.ID, "worker", "written once the issue was bound")
	if err != nil {
		t.Fatal(err)
	}
	op2, ok := ytcOpFor(t, s, c2.ID)
	if !ok || op2.YTTargetID != ytrqIssue {
		t.Fatalf("targeted op %+v %v", op2, ok)
	}
	ytrqMoveTask(t, s, x.ID, ytrqOther)
	mv := ytrqPrepareMove(t, s, x.ID, ytrqOtherID)

	ytrqRejectedAmbiguous(t, s, op1)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with the move pending: %+v", got)
	}
	if linked, ok := ytcOpFor(t, s, c1.ID); !ok || linked.ID != op1.ID {
		t.Fatalf("link moved before the binding agreed: %+v", linked)
	}
	fake := ytwritetest.NewFake()
	counting := &ytcCounting{Remote: fake}
	var offset atomic.Int64
	if got := ytqDrive(t, s, ytcProtocol(t, s, counting, &offset), op2.ID); got.State != ytwrite.StateApplied {
		t.Fatalf("targeted comment %+v", got)
	}
	s.SetYTMovesMode(YTMovesOn)
	ytmSettle(t, s, mv, ytrqIssue)
	if project, _ := ytmBinding(t, s, x.ID); project != ytrqOtherID {
		t.Fatalf("binding project after the move %q", project)
	}

	again := ytrqWantRequeued(t, s, ytrqRequeue(t, s), c1, op1, mv, op2)
	if want := "ytwrite.comment_requeued op_id=" + strconv.FormatInt(again.ID, 10) + " from_op_id=" + strconv.FormatInt(op1.ID, 10); !contains(logs(), want) {
		t.Fatalf("log %q, want %q", logs(), want)
	}
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("a second pass requeued again: %+v", got)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND entity_id=$3`,
		YTCommentPrincipal, ytwrite.KindCommentAppend, strconv.FormatInt(x.ID, 10)); n != 3 {
		t.Fatalf("comment ops on the task = %d, want 3 (C1, C2, C1 again)", n)
	}
	s.SetYTMovesMode(YTMovesOff)
	ytrqDeliver(t, s, fake, again, c1.Body)
	if cs := fake.Comments(ytrqIssue); len(cs) != 2 || !contains(cs[0].Text, c2.Body) {
		t.Fatalf("issue comments %+v, want C2 then C1", cs)
	}
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued after delivery: %+v", got)
	}
}

// A task that moves away while its create is in flight and comes back before
// anything moves the issue: no move op ever exists, and the comment is copied
// again once the binding agrees.
func TestYTCommentRequeuedWhenTheTaskComesBack(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_back")
	ytrqMapped(t, s)
	c1, op1 := ytrqQueued(t, s, x.ID, "queued, the task left and came back")
	ytiDispatch(t, s, create)
	ytrqMoveTask(t, s, x.ID, ytrqOther)
	ytrqApplyCreate(t, s, create)
	ytrqRejectedAmbiguous(t, s, op1)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued while the task is elsewhere: %+v", got)
	}
	ytrqMoveTask(t, s, x.ID, ytbProject)
	again := ytrqWantRequeued(t, s, ytrqRequeue(t, s), c1, op1)
	if ops := ytmOps(t, s, x.ID); len(ops) != 0 {
		t.Fatalf("move ops %+v", ops)
	}
	ytrqDeliver(t, s, ytwritetest.NewFake(), again, c1.Body)
}

// A move still unresolved on the lane (review r4573479597): the comment is
// rejected while the task is in B, a move to B is prepared, and the task comes
// back to A before that move applies. The binding and the task both say A,
// but the move to B would apply before a copy, so nothing is copied while it
// is pending, nor after it applies (the binding is in B, the task in A); the
// comment is copied once the move back to A applies, after both moves.
func TestYTCommentRequeueWaitsForAPendingMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_pending_move")
	ytrqMapped(t, s)
	c1, op1 := ytrqQueued(t, s, x.ID, "queued, the task left and came back with a move pending")
	ytiDispatch(t, s, create)
	ytrqMoveTask(t, s, x.ID, ytrqOther)
	ytrqApplyCreate(t, s, create)
	away := ytrqPrepareMove(t, s, x.ID, ytrqOtherID)
	ytrqRejectedAmbiguous(t, s, op1)
	ytrqMoveTask(t, s, x.ID, ytbProject)
	if project, _ := ytmBinding(t, s, x.ID); project != ytbProjectID {
		t.Fatalf("binding project with the move pending %q", project)
	}
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with a move to %s pending: %+v", ytrqOtherID, got)
	}
	if _, done, err := s.requeueYTComment(t.Context(), op1.ID, c1.ID, x.ID); err != nil || done {
		t.Fatalf("requeue under the lane lock with a move pending: done=%v err=%v", done, err)
	}
	if linked, ok := ytcOpFor(t, s, c1.ID); !ok || linked.ID != op1.ID {
		t.Fatalf("link moved with a move pending: %+v", linked)
	}
	s.SetYTMovesMode(YTMovesOn)
	ytmSettle(t, s, away, ytrqIssue)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with the binding in B and the task in A: %+v", got)
	}
	back := ytrqPrepareMove(t, s, x.ID, ytbProjectID)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with the move back pending: %+v", got)
	}
	ytmSettle(t, s, back, ytrqIssue)
	again := ytrqWantRequeued(t, s, ytrqRequeue(t, s), c1, op1, away, back)
	s.SetYTMovesMode(YTMovesOff)
	ytrqDeliver(t, s, ytwritetest.NewFake(), again, c1.Body)
}

// Two moves (r4564127096): the task moves to B, then to C, before the queued
// comment's claim. After move A applies, the binding (in B) still disagrees
// with the task (in C) and nothing is copied; after move B applies it agrees
// and the comment is copied after both moves.
func TestYTCommentRequeuedAfterTheLastMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_two_moves")
	ytrqMapped(t, s)
	c1, op1 := ytrqQueued(t, s, x.ID, "queued across two moves")
	ytiDispatch(t, s, create)
	ytrqMoveTask(t, s, x.ID, ytrqOther)
	ytrqApplyCreate(t, s, create)
	moveA := ytrqPrepareMove(t, s, x.ID, ytrqOtherID)
	ytrqMoveTask(t, s, x.ID, ytrqThird)
	moveB := ytrqPrepareMove(t, s, x.ID, ytrqThirdID)

	ytrqRejectedAmbiguous(t, s, op1)
	s.SetYTMovesMode(YTMovesOn)
	ytmSettle(t, s, moveA, ytrqIssue)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with the binding in B and the task in C: %+v", got)
	}
	ytmSettle(t, s, moveB, ytrqIssue)
	again := ytrqWantRequeued(t, s, ytrqRequeue(t, s), c1, op1, moveA, moveB)
	s.SetYTMovesMode(YTMovesOff)
	ytrqDeliver(t, s, ytwritetest.NewFake(), again, c1.Body)
}

// With fields and moves on, the create's apply prepares the move and the
// snapshot behind the queued comment (ytFieldsOnBindTx; r4563234442 and
// r4564123083 had a snapshot between the comment and the move). The comment
// is rejected at its claim, so the move and the snapshot are claimable in
// turn, and the comment is copied after both.
func TestYTCommentRequeuedAfterTheSnapshot(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_snapshot")
	ytrqMapped(t, s)
	ctx := t.Context()
	c1, op1 := ytrqQueued(t, s, x.ID, "queued with fields on")
	ytiDispatch(t, s, create)
	ytrqMoveTask(t, s, x.ID, ytrqOther)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	s.SetYTMovesMode(YTMovesOn)
	ytrqApplyCreate(t, s, create)
	moves := ytmOps(t, s, x.ID)
	snaps, err := s.YTWriteLedger().list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 ORDER BY id`,
		ytwrite.KindIssueFields, strconv.FormatInt(x.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || len(snaps) != 1 || moves[0].ID <= op1.ID || snaps[0].ID <= op1.ID {
		t.Fatalf("ops prepared by the create's apply: moves %+v snapshots %+v (comment %d)", moves, snaps, op1.ID)
	}

	ytrqRejectedAmbiguous(t, s, op1)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued with the move pending: %+v", got)
	}
	first, second := moves[0], snaps[0]
	if second.ID < first.ID {
		first, second = second, first
	}
	ytmSettle(t, s, first, ytrqIssue)
	ytmSettle(t, s, second, ytrqIssue)
	again := ytrqWantRequeued(t, s, ytrqRequeue(t, s), c1, op1, moves[0], snaps[0])
	s.SetYTFieldsMode(YTFieldsOff, "")
	s.SetYTMovesMode(YTMovesOff)
	ytrqDeliver(t, s, ytwritetest.NewFake(), again, c1.Body)
}

// Only a comment rejected ambiguous_issue at its claim is copied again: one
// whose create was rejected (issue_not_bound) has no binding to go to, and
// the pass leaves it alone, as it does in a mode other than on and for
// another tenant.
func TestYTCommentRequeueScope(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrq_scope")
	ytrqMapped(t, s)
	_, op1 := ytrqQueued(t, s, x.ID, "queued behind a create that fails")
	ytmReject(t, s, create)
	if out := ytiClaim(t, s, op1); out != ytwrite.ClaimNotPrepared {
		t.Fatalf("claim = %s", out)
	}
	if got, err := s.YTWriteLedger().ByID(t.Context(), op1.ID); err != nil || got.Reason != ytwrite.ReasonIssueNotBound {
		t.Fatalf("queued op %+v %v", got, err)
	}
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued an unbound comment: %+v", got)
	}

	// An ambiguous one that agrees now, seen by another tenant's worker and
	// outside mode on: nothing.
	y := ytiCreate(t, s, ytbProject)
	ycreate := ytiOps(t, s, y.ID)[0]
	_, yop := ytrqQueued(t, s, y.ID, "queued, the task left and came back")
	ytiDispatch(t, s, ycreate)
	ytrqMoveTask(t, s, y.ID, ytrqOther)
	ytrqApplyCreate(t, s, ycreate)
	ytrqRejectedAmbiguous(t, s, yop)
	ytrqMoveTask(t, s, y.ID, ytbProject)
	if got, err := s.RequeueYTMovedComments(t.Context(), "other.youtrack.cloud", 100); err != nil || len(got) != 0 {
		t.Fatalf("another tenant's pass: %+v %v", got, err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued in mode canary: %+v", got)
	}
	s.SetYTCommentsMode(YTCommentsOn)

	// A comment at or below the binding's comments_from is one the issue
	// already has: nothing.
	var ycID int64
	if err := s.pool.QueryRow(t.Context(), `SELECT comment_id FROM yt_comment_write_links WHERE op_id=$1`, yop.ID).Scan(&ycID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_task_bindings SET comments_from=$2 WHERE task_id=$1`, y.ID, ycID); err != nil {
		t.Fatal(err)
	}
	if got := ytrqRequeue(t, s); len(got) != 0 {
		t.Fatalf("requeued a comment at comments_from: %+v", got)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_task_bindings SET comments_from=0 WHERE task_id=$1`, y.ID); err != nil {
		t.Fatal(err)
	}

	// A rejected ambiguous_issue op that has a target was never a queued
	// comment: nothing.
	tc, err := s.CreateTaskComment(t.Context(), y.ID, "worker", "targeted, then refused")
	if err != nil {
		t.Fatal(err)
	}
	top, ok := ytcOpFor(t, s, tc.ID)
	if !ok || top.YTTargetID != ytrqIssue {
		t.Fatalf("targeted op %+v %v", top, ok)
	}
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rejectYTOpTx(t.Context(), tx, time.Now().UTC(), top.ID, ytwrite.ReasonAmbiguousIssue, "test"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := ytrqRequeue(t, s); len(got) != 1 || got[0].From != yop.ID {
		t.Fatalf("requeued in mode on: %+v (want only op %d, not the targeted op %d)", got, yop.ID, top.ID)
	}
	if again := ytrqRequeue(t, s); len(again) != 0 {
		t.Fatalf("second pass: %+v", again)
	}
}

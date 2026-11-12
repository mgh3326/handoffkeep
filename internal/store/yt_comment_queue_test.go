package store

import (
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// hk 4263: under comments authority youtrack, a comment on a task whose
// issue_create op is still pending is accepted with an op queued behind the
// create, which takes its target from the binding the create publishes.

// ytqSetup is a store in comments mode on and issue mode on, with comments
// canonical in YouTrack, and a task filed through CreateTask (so it has a
// prepared issue_create op and no binding).
func ytqSetup(t *testing.T, prefix string) (*Store, Task, ytwrite.Op) {
	t.Helper()
	s := schemaStore(t, prefix)
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	if _, _, err := s.SetYTAuthority(t.Context(), []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "operator", "ytq"); err != nil {
		t.Fatal(err)
	}
	x := ytiCreate(t, s, ytbProject)
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 1 || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("issue ops %+v", ops)
	}
	return s, x, ops[0]
}

// ytqDrive runs the comment worker until op is resolved (a few passes: the
// create, then the comment behind it).
func ytqDrive(t *testing.T, s *Store, p *ytwrite.Protocol, opID int64) ytwrite.Op {
	t.Helper()
	for range 5 {
		if _, err := p.Drive(t.Context(), 20); err != nil {
			t.Fatal(err)
		}
		op, err := s.YTWriteLedger().ByID(t.Context(), opID)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == ytwrite.StateApplied || op.State == ytwrite.StateRejected {
			return op
		}
	}
	op, _ := s.YTWriteLedger().ByID(t.Context(), opID)
	t.Fatalf("op %d not resolved: %+v", opID, op)
	return op
}

func TestYTCommentQueuedBehindIssueCreate(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_happy")
	ctx := t.Context()
	logs := ytbLog(t)

	c, err := s.CreateTaskCommentProjected(ctx, x.ID, "worker", "filed and commented at once")
	if err != nil {
		t.Fatalf("comment on a task whose issue is being created: %v", err)
	}
	if c.YouTrackProjection == nil || c.YouTrackProjection.Status != string(ytwrite.StatusPending) {
		t.Fatalf("projection %+v", c.YouTrackProjection)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared || op.Kind != ytwrite.KindCommentAppend || op.Principal != YTCommentPrincipal ||
		op.Key != YTCommentKey(c.ID) || op.YTTargetID != "" || op.Tenant != ytcTenant || op.EntityKind != "task" ||
		op.EntityID != strconv.FormatInt(x.ID, 10) || op.ID <= create.ID || c.YouTrackProjection.OpID != op.ID {
		t.Fatalf("queued op %+v %v (create %d)", op, ok, create.ID)
	}
	if want := "ytwrite.comment_queued op_id=" + strconv.FormatInt(op.ID, 10); !contains(logs(), want) {
		t.Fatalf("log %q, want %q", logs(), want)
	}
	// Behind the create on the task's lane: never claimed first.
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimBlocked {
		t.Fatalf("claim before the create = %s", out)
	}
	// No map anchor: the create's apply writes the task's map row.
	if _, ok, _ := s.YTWriteLedger().MapGet(ctx, "task", op.EntityID); ok {
		t.Fatal("the queued comment anchored the task's map row")
	}

	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	got := ytqDrive(t, s, p, op.ID)
	b, found, err := s.YTTaskBinding(ctx, x.ID)
	if err != nil || !found || b.OpID != create.ID {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	if got.State != ytwrite.StateApplied || got.YTTargetID != b.IssueID || got.Tenant != ytcTenant {
		t.Fatalf("comment op %+v, binding %+v", got, b)
	}
	// The hash covers the target now, as an op prepared against the
	// binding would; the key and the render are unchanged.
	want, err := ytwrite.CommentOp(ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: "worker",
		TaskID: x.ID, YTIssueID: b.IssueID, Tenant: ytcTenant, Body: c.Body})
	if err != nil {
		t.Fatal(err)
	}
	if got.PayloadSHA != want.PayloadSHA || got.Key != op.Key || string(got.Payload) != string(op.Payload) {
		t.Fatalf("op %+v, want sha %s", got, want.PayloadSHA)
	}
	comments := yt.Comments(b.IssueID)
	text, err := ytwrite.RenderComment(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Text != text {
		t.Fatalf("issue %s comments %+v, want one %q", b.IssueID, comments, text)
	}
	if echo, err := s.YTCommentEchoOp(ctx, got.ID, got.Tenant, b.IssueID); err != nil || !echo {
		t.Fatalf("echo evidence = %v %v", echo, err)
	}
	// The catch-up sees the link and prepares nothing more.
	if up := ytcuPass(t, s, 100, nil); len(up.Prepared) != 0 {
		t.Fatalf("catch-up prepared %+v", up.Prepared)
	}
}

// A create that ends rejected publishes no binding: the queued op is claimed
// with no target, refused before any YouTrack call, and alerted with the
// rejected ops.
func TestYTCommentQueuedCreateRejected(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_rejected")
	ctx := t.Context()
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued then orphaned")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("queued op %+v %v", op, ok)
	}
	if _, err := s.YTWriteLedger().Reject(ctx, time.Now().UTC(), create.ID, ytwrite.ReasonAmbiguousIssue, "test"); err != nil {
		t.Fatal(err)
	}
	yt := ytwritetest.NewFake()
	counting := &ytcCounting{Remote: yt}
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, counting, &offset), op.ID)
	// Rejected by the claim itself: never dispatched, so no stale recovery
	// can leave it remote_unknown on the lane.
	if got.State != ytwrite.StateRejected || got.Reason != ytwrite.ReasonIssueNotBound || got.YTTargetID != "" || got.Attempts != 0 {
		t.Fatalf("comment op %+v", got)
	}
	if n := counting.posts.Load(); n != 0 {
		t.Fatalf("%d comment posts for an op with no target", n)
	}
	if n, err := s.EnqueueYTWriteOpAlerts(ctx, time.Now().UTC()); err != nil || n < 1 {
		t.Fatalf("alerts = %d %v", n, err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_op_alerts WHERE op_id=$1 AND alert_class='rejected'`, got.ID); n != 1 {
		t.Fatalf("rejected alert rows for op %d = %d", got.ID, n)
	}
}

// A create applied with binding_conflict publishes no binding either: the
// queued op is rejected the same way and nothing reaches the conflicting
// issue.
func TestYTCommentQueuedCreateBindingConflict(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_conflict")
	ctx := t.Context()
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued behind a conflicting create")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok {
		t.Fatal("no queued op")
	}
	// The task's map row names another issue: the create's apply finds the
	// conflict and publishes nothing.
	ytbAnchor(t, s, x.ID, "2-777")
	ytiDispatch(t, s, create)
	applied, err := s.YTWriteLedger().Apply(ctx, time.Now().UTC(), create.ID, "2-888", ytiMaps(create, "2-888"))
	if err != nil || applied.Reason != ytwrite.ReasonBindingConflict {
		t.Fatalf("create apply %+v %v", applied, err)
	}
	yt := ytwritetest.NewFake()
	counting := &ytcCounting{Remote: yt}
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, counting, &offset), op.ID)
	if got.State != ytwrite.StateRejected || got.Reason != ytwrite.ReasonIssueNotBound || got.YTTargetID != "" || got.Attempts != 0 || counting.posts.Load() != 0 {
		t.Fatalf("comment op %+v, posts %d", got, counting.posts.Load())
	}
}

// Every other unsendable case is still refused under youtrack, and hk
// authority is unchanged.
func TestYTCommentQueuedOnlyBehindAPendingCreate(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_refused")
	ctx := t.Context()

	// No issue_create op at all.
	bare := ytcTask(t, s, ytbProject)
	if _, err := s.CreateTaskComment(ctx, bare, "worker", "no create"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("task with no create: err = %v", err)
	}
	// A body the protocol cannot send.
	if _, err := s.CreateTaskComment(ctx, x.ID, "worker", "quotes <!-- hk-op:12 -->"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("marker body: err = %v", err)
	}
	// Mode canary queues nothing.
	s.SetYTCommentsMode(YTCommentsCanary)
	if _, err := s.CreateTaskComment(ctx, x.ID, "worker", "canary"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("mode canary: err = %v", err)
	}
	s.SetYTCommentsMode(YTCommentsOn)
	// A create that already ended rejected queues nothing.
	if _, err := s.YTWriteLedger().Reject(ctx, time.Now().UTC(), create.ID, ytwrite.ReasonAmbiguousIssue, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskComment(ctx, x.ID, "worker", "after the create failed"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("rejected create: err = %v", err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind=$1`, ytwrite.KindCommentAppend); n != 0 {
		t.Fatalf("%d comment ops after refusals", n)
	}

	// Under hk the same comment on a task with a pending create commits
	// with no op, as before; the catch-up copies it once the task is bound.
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "operator", "rollback"); err != nil {
		t.Fatal(err)
	}
	y := ytiCreate(t, s, ytbProject)
	c, err := s.CreateTaskComment(ctx, y.ID, "worker", "hk authority")
	if err != nil {
		t.Fatal(err)
	}
	if op, ok := ytcOpFor(t, s, c.ID); ok {
		t.Fatalf("hk authority queued op %+v", op)
	}
}

// The guard lets a queued comment op receive its target once, in the update
// that dispatches it, and nothing else.
func TestYTWriteOpsGuardQueuedTarget(t *testing.T) {
	s := schemaStore(t, "ytq_guard")
	ctx := t.Context()
	now := time.Now().UTC()
	mk := func(key, target string) ytwrite.Op {
		t.Helper()
		op, _, err := s.YTWriteLedger().Prepare(ctx, now, ytwrite.NewOp{Principal: YTCommentPrincipal, Kind: ytwrite.KindCommentAppend, Key: key,
			PayloadSHA: "s" + key, Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "ytq-" + key, YTTargetID: target, Actor: "a", Tenant: ytcTenant})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	exec := func(q string, args ...any) error {
		_, err := s.pool.Exec(ctx, q, args...)
		return err
	}
	queued := mk("q", "")
	for _, q := range []string{
		`UPDATE yt_write_ops SET yt_target_id='2-1' WHERE id=$1`,                                             // not with the dispatch
		`UPDATE yt_write_ops SET state='dispatched', payload='{"body":"y"}', yt_target_id='2-1' WHERE id=$1`, // payload stays
		`UPDATE yt_write_ops SET state='dispatched', actor='b', yt_target_id='2-1' WHERE id=$1`,
	} {
		if err := exec(q, queued.ID); err == nil || !contains(err.Error(), "immutable") {
			t.Fatalf("%s: err = %v", q, err)
		}
	}
	if err := exec(`UPDATE yt_write_ops SET state='dispatched', yt_target_id='2-1', payload_sha='t', dispatched_at=now() WHERE id=$1`, queued.ID); err != nil {
		t.Fatalf("retarget at dispatch: %v", err)
	}
	// Once set, never again.
	if err := exec(`UPDATE yt_write_ops SET state='prepared', yt_target_id='2-2' WHERE id=$1`, queued.ID); err == nil || !contains(err.Error(), "immutable") {
		t.Fatalf("second retarget: err = %v", err)
	}
	targeted := mk("t", "2-5")
	if err := exec(`UPDATE yt_write_ops SET state='dispatched', yt_target_id='2-6', dispatched_at=now() WHERE id=$1`, targeted.ID); err == nil || !contains(err.Error(), "immutable") {
		t.Fatalf("retarget of a targeted op: err = %v", err)
	}
}

// A binding the create publishes is checked against every local source at the
// claim, as a fresh comment's is: a task moved to another project while its
// create was in flight keeps a binding in the old project, and the queued
// comment is rejected instead of sent there.
func TestYTCommentQueuedBindingDisagreement(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_moved")
	ctx := t.Context()
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued, then the task moved")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok {
		t.Fatal("no queued op")
	}
	const other = "ytq-other"
	if _, err := s.AddTaskProject(ctx, other, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: other, ProjectID: "0-9", ShortName: "HKO"}); err != nil {
		t.Fatal(err)
	}
	// Moved while the create is in flight: a prepared op would be replaced
	// at its claim (hk 4318), a dispatched one keeps its project.
	ytiDispatch(t, s, create)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, x.ID, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.YTWriteLedger().Apply(ctx, time.Now().UTC(), create.ID, "2-900", ytiMaps(create, "2-900")); err != nil {
		t.Fatal(err)
	}
	if b, found, err := s.YTTaskBinding(ctx, x.ID); err != nil || !found || b.IssueID != "2-900" {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	counting := &ytcCounting{Remote: ytwritetest.NewFake()}
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, counting, &offset), op.ID)
	if got.State != ytwrite.StateRejected || got.Reason != ytwrite.ReasonAmbiguousIssue || got.YTTargetID != "" || got.Attempts != 0 || counting.posts.Load() != 0 {
		t.Fatalf("comment op %+v, posts %d", got, counting.posts.Load())
	}
}

// hk 4318: a comment queued behind a task's issue op follows the task's
// active issue op, whatever its key, when the task moves before the op is
// sent.

// ytqDue reports whether the tenant worker's Due lists op id now.
func ytqDue(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	due, err := s.YTTenantLedger(ytcTenant).Due(t.Context(), time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range due {
		if op.ID == id {
			return true
		}
	}
	return false
}

// The ESC's sequence (PR #335 round 3): comment op 2 is queued behind issue
// op 1; the task moves, which rejects op 1 and prepares op 3 (id above op
// 2's). Op 2 waits for op 3 instead of being claimed first and rejected
// issue_not_bound, op 3 does not wait for op 2, and the comment is delivered
// to op 3's issue in the new project.
func TestYTCommentQueuedFollowsTheReplacementAfterAMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_replaced")
	s.SetYTIssueRetarget(true)
	ctx := t.Context()
	ytrProject(t, s, "ytq-other", "0-9")
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued behind op 1")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared || op.YTTargetID != "" || op.ID <= create.ID {
		t.Fatalf("queued op %+v %v", op, ok)
	}
	ytrMove(t, s, x.ID, "ytq-other")
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[0].ID != create.ID || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared || ops[1].ID <= op.ID {
		t.Fatalf("issue ops after the move %+v (comment op %d)", ops, op.ID)
	}
	replacement := ops[1]
	// Claim and Due agree: the comment waits, the replacement goes first.
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimBlocked {
		t.Fatalf("claim of the comment before the replacement = %s", out)
	}
	if ytqDue(t, s, op.ID) || !ytqDue(t, s, replacement.ID) {
		t.Fatalf("due: comment %t, replacement %t", ytqDue(t, s, op.ID), ytqDue(t, s, replacement.ID))
	}

	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, yt, &offset), op.ID)
	b, found, err := s.YTTaskBinding(ctx, x.ID)
	if err != nil || !found || b.OpID != replacement.ID || b.ProjectID != "0-9" {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	if got.State != ytwrite.StateApplied || got.YTTargetID != b.IssueID || got.Tenant != ytcTenant {
		t.Fatalf("comment op %+v, binding %+v", got, b)
	}
	if n := len(yt.Issues(ytbProjectID)); n != 0 {
		t.Fatalf("%d issues created in the old project", n)
	}
	if comments := yt.Comments(b.IssueID); len(comments) != 1 {
		t.Fatalf("issue %s comments %+v", b.IssueID, comments)
	}
}

// While the replacement is pending, a new comment is queued behind it, not
// refused; the comments keep their order across the replacement.
func TestYTCommentQueuedKeepsOrderAcrossAReplacement(t *testing.T) {
	s, x, _ := ytqSetup(t, "ytq_order")
	s.SetYTIssueRetarget(true)
	ctx := t.Context()
	ytrProject(t, s, "ytq-other", "0-9")
	first, err := s.CreateTaskComment(ctx, x.ID, "worker", "first, behind op 1")
	if err != nil {
		t.Fatal(err)
	}
	ytrMove(t, s, x.ID, "ytq-other")
	replacement := ytiOps(t, s, x.ID)[1]
	second, err := s.CreateTaskComment(ctx, x.ID, "worker", "second, behind the replacement")
	if err != nil {
		t.Fatalf("comment while the replacement is pending: %v", err)
	}
	op1, _ := ytcOpFor(t, s, first.ID)
	op2, ok := ytcOpFor(t, s, second.ID)
	if !ok || op2.State != ytwrite.StatePrepared || op2.YTTargetID != "" || op2.Tenant != replacement.Tenant || op2.ID <= replacement.ID {
		t.Fatalf("second op %+v %v (replacement %+v)", op2, ok, replacement)
	}
	for _, op := range []ytwrite.Op{op1, op2} {
		if out := ytiClaim(t, s, op); out != ytwrite.ClaimBlocked {
			t.Fatalf("claim of comment op %d before the replacement = %s", op.ID, out)
		}
	}
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	got2 := ytqDrive(t, s, p, op2.ID)
	got1, err := s.YTWriteLedger().ByID(ctx, op1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.State != ytwrite.StateApplied || got2.State != ytwrite.StateApplied || got1.YTTargetID != got2.YTTargetID {
		t.Fatalf("comment ops %+v %+v", got1, got2)
	}
	text1, err := ytwrite.RenderComment(got1)
	if err != nil {
		t.Fatal(err)
	}
	text2, err := ytwrite.RenderComment(got2)
	if err != nil {
		t.Fatal(err)
	}
	comments := yt.Comments(got1.YTTargetID)
	if len(comments) != 2 || comments[0].Text != text1 || comments[1].Text != text2 {
		t.Fatalf("issue %s comments %+v", got1.YTTargetID, comments)
	}
}

// A move to an unmapped project leaves an intent instead of a replacement:
// the queued comment waits for it (never rejected issue_not_bound) and is
// delivered once the project is mapped and the intent becomes the op.
func TestYTCommentQueuedWaitsForTheIssueIntent(t *testing.T) {
	s, x, _ := ytqSetup(t, "ytq_intent")
	s.SetYTIssueRetarget(true)
	ctx := t.Context()
	ytrProject(t, s, "ytq-later", "")
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued, then the task moved to an unmapped project")
	if err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, c.ID)
	ytrMove(t, s, x.ID, "ytq-later")
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].Reason != ytwrite.ReasonProjectChanged || ytrIntent(t, s, x.ID) != ytcTenant {
		t.Fatalf("ops %+v, intent %q", ops, ytrIntent(t, s, x.ID))
	}
	// The fake itself, not ytcCounting: the protocol needs its issue calls.
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	for range 3 {
		if _, err := p.Drive(ctx, 20); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.YTWriteLedger().ByID(ctx, op.ID); err != nil || got.State != ytwrite.StatePrepared || yt.Calls() != 0 {
		t.Fatalf("comment op while the intent waits %+v %v, remote calls %d", got, err, yt.Calls())
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytq-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(ctx, 10); err != nil || n != 1 {
		t.Fatalf("intent pass prepared %d, %v", n, err)
	}
	got := ytqDrive(t, s, p, op.ID)
	b, found, err := s.YTTaskBinding(ctx, x.ID)
	if err != nil || !found || b.ProjectID != "0-11" || got.State != ytwrite.StateApplied || got.YTTargetID != b.IssueID {
		t.Fatalf("comment op %+v, binding %+v %v %v", got, b, found, err)
	}
}

// A binding the operator verifies while a comment is queued behind an issue
// intent is refused (PR #357 review): the queued comment takes its target
// only from a binding an issue_create op publishes, so it would be rejected.
// Once the intent's op applies, the comment is delivered.
func TestYTCommentQueuedRefusesABindingWhileItWaits(t *testing.T) {
	s, x, _ := ytqSetup(t, "ytq_bind")
	s.SetYTIssueRetarget(true)
	ctx := t.Context()
	ytrProject(t, s, "ytq-later", "")
	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "queued, then the task moved to an unmapped project")
	if err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, c.ID)
	ytrMove(t, s, x.ID, "ytq-later")
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytq-later", ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	b := ytbBinding(x.ID)
	b.ProjectID = "0-11"
	if _, err := ytbAdd(t, s, b); !errors.Is(err, ErrYTBindingConflict) || !contains(err.Error(), "unresolved") {
		t.Fatalf("binding while a comment is queued: err = %v", err)
	}
	if n, err := s.PrepareMappedYTIssues(ctx, 10); err != nil || n != 1 {
		t.Fatalf("intent pass prepared %d, %v", n, err)
	}
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, yt, &offset), op.ID)
	if got.State != ytwrite.StateApplied || got.YTTargetID == "" {
		t.Fatalf("comment op %+v", got)
	}
}

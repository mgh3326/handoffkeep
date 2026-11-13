package store

import (
	"errors"
	"slices"
	"strconv"
	"strings"
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

// ytqHeld is the hk 4457 sequence up to the hold: a comment queued behind
// the task's issue_create, the task moved while the create was in flight, the
// create applied as issue 2-900 in the old project. The comment's first claim
// holds it. It returns the held comment op.
func ytqHeld(t *testing.T, prefix string) (*Store, Task, ytwrite.Op) {
	t.Helper()
	s, x, create := ytqSetup(t, prefix)
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
	// The task moves while the create is in flight (a prepared op would
	// instead be dispatched to the new project at its claim, hk 4318).
	ytiDispatch(t, s, create)
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, x.ID, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.YTWriteLedger().Apply(ctx, time.Now().UTC(), create.ID, "2-900", ytiMaps(create, "2-900")); err != nil {
		t.Fatal(err)
	}
	if b, found, err := s.YTTaskBinding(ctx, x.ID); err != nil || !found || b.IssueID != "2-900" || b.ProjectID != ytbProjectID {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimHeld {
		t.Fatalf("claim with the issue in the old project = %s", out)
	}
	return s, x, op
}

// ytqMove prepares the issue_move the fields feed prepares for a task whose
// issue must move (ytMoveForTx), with moves mode on.
func ytqMove(t *testing.T, s *Store, task int64, issue, projectID string) ytwrite.Op {
	t.Helper()
	s.SetYTMovesMode(YTMovesOn)
	n, err := ytwrite.MoveOp(ytwrite.MoveRequest{Principal: YTFieldsPrincipal, Actor: YTFieldsPrincipal, Tenant: ytcTenant,
		IssueID: issue, ProjectID: projectID, TaskID: task})
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

// A binding the create publishes is checked against every local source at the
// claim, as a fresh comment's is: a task moved to another project while its
// create was pending keeps a binding in the old project. The queued comment is
// held, not rejected; the issue_move the feed prepares behind it passes it on
// the lane, and once the move places the issue in the task's project the same
// op is claimed and delivered there (hk 4457).
func TestYTCommentQueuedBindingDisagreement(t *testing.T) {
	logs := ytbLog(t)
	s, x, op := ytqHeld(t, "ytq_moved")
	ctx := t.Context()
	// The claim held the op: still prepared, no target, nothing sent.
	held, err := s.YTWriteLedger().ByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != ytwrite.StatePrepared || held.YTTargetID != "" || held.Attempts != 0 ||
		!held.NextAttemptAt.After(time.Now().UTC().Add(30*time.Second)) {
		t.Fatalf("held op %+v", held)
	}
	if want := "ytwrite.comment_queued_held op_id=" + strconv.FormatInt(op.ID, 10); !contains(logs(), want) {
		t.Fatalf("log %q, want %q", logs(), want)
	}
	if _, err := s.EnqueueYTWriteOpAlerts(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_op_alerts WHERE op_id=$1 AND alert_class='rejected'`, op.ID); n != 0 {
		t.Fatalf("a held op was alerted as rejected: %d rows", n)
	}

	// The move comes after the held comment on the task's lane, and is due
	// and claimed anyway; its apply writes the binding's project.
	mv := ytqMove(t, s, x.ID, "2-900", "0-9")
	if mv.ID <= op.ID {
		t.Fatalf("move %d before the held comment %d", mv.ID, op.ID)
	}
	due, err := s.YTTenantLedger(ytcTenant).Due(ctx, time.Now().UTC(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(due, func(o ytwrite.Op) bool { return o.ID == mv.ID }) {
		t.Fatalf("move %d not due behind the held comment: %+v", mv.ID, due)
	}
	// While the move is pending the comment stays held, past the grace too.
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(ctx, time.Now().UTC().Add(2*ytCommentHoldGrace), op.ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim of the comment with the move pending = %s %v", out, err)
	}
	ytmSettle(t, s, mv, "2-900")
	if project, _ := ytmBinding(t, s, x.ID); project != "0-9" {
		t.Fatalf("binding project after the move %q", project)
	}

	// The held op is due again and delivered there.
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	offset.Store(int64(3 * ytCommentHoldGrace))
	got := ytqDrive(t, s, ytcProtocol(t, s, yt, &offset), op.ID)
	if got.State != ytwrite.StateApplied || got.YTTargetID != "2-900" || got.Attempts != 1 {
		t.Fatalf("comment op after the move %+v", got)
	}
	if comments := yt.Comments("2-900"); len(comments) != 1 || !strings.Contains(comments[0].Text, "queued, then the task moved") {
		t.Fatalf("issue 2-900 comments %+v", comments)
	}
}

// The review's cycle (PR #383): an issue_fields snapshot the feed prepared
// for the issue's old project sits between the held comment and the move
// that releases it. The snapshot passes the held comment, the move then
// passes it too, and once both are applied the comment is delivered; neither
// waits for the comment while the comment waits for the move.
func TestYTCommentQueuedHoldLetsTheLaneDrain(t *testing.T) {
	s, x, op := ytqHeld(t, "ytq_hold_drain")
	ctx := t.Context()
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	n, err := ytwrite.FieldsOp(ytwrite.FieldsRequest{Principal: YTFieldsPrincipal, Actor: YTFieldsPrincipal, Tenant: ytcTenant,
		IssueID: "2-900", ProjectID: ytbProjectID, Task: ytwrite.TaskFields{TaskID: x.ID, Title: x.Title, State: "todo"}})
	if err != nil {
		t.Fatal(err)
	}
	n.Key = SnapshotKey(ytFieldsConsumer, strconv.FormatInt(x.ID, 10), 1, n.PayloadSHA)
	fields, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), n)
	if err != nil {
		t.Fatal(err)
	}
	mv := ytqMove(t, s, x.ID, "2-900", "0-9")
	if !(op.ID < fields.ID && fields.ID < mv.ID) {
		t.Fatalf("lane order comment %d, fields %d, move %d", op.ID, fields.ID, mv.ID)
	}
	l := s.YTTenantLedger(ytcTenant)
	ids := func() []int64 {
		due, err := l.Due(ctx, time.Now().UTC().Add(2*ytCommentHoldWait), 20)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, o := range due {
			if o.EntityID == strconv.FormatInt(x.ID, 10) {
				out = append(out, o.ID)
			}
		}
		return out
	}
	// The snapshot is due behind the held comment; the move still waits for
	// the snapshot, as lane order says.
	if got := ids(); !slices.Contains(got, fields.ID) || slices.Contains(got, mv.ID) {
		t.Fatalf("due %v, want the snapshot %d and not the move %d", got, fields.ID, mv.ID)
	}
	ytmSettle(t, s, fields, "2-900")
	if got := ids(); !slices.Contains(got, mv.ID) {
		t.Fatalf("due %v, want the move %d once the snapshot applied", got, mv.ID)
	}
	if _, out, err := l.Claim(ctx, time.Now().UTC().Add(2*ytCommentHoldGrace), op.ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim of the comment with the move pending = %s %v", out, err)
	}
	ytmSettle(t, s, mv, "2-900")
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	offset.Store(int64(3 * ytCommentHoldGrace))
	if got := ytqDrive(t, s, ytcProtocol(t, s, yt, &offset), op.ID); got.State != ytwrite.StateApplied || got.YTTargetID != "2-900" {
		t.Fatalf("comment op after the snapshot and the move %+v", got)
	}
}

// A hold no move explains is bounded: past ytCommentHoldGrace with no
// issue_move pending (moves mode off, the move refused), the comment is
// rejected ambiguous_issue as before hk 4457, and the lane clears.
func TestYTCommentQueuedHoldIsBounded(t *testing.T) {
	s, _, op := ytqHeld(t, "ytq_hold_bounded")
	ctx := t.Context()
	l := s.YTTenantLedger(ytcTenant)
	if _, out, err := l.Claim(ctx, time.Now().UTC().Add(2*time.Minute), op.ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim within the grace = %s %v", out, err)
	}
	if _, _, err := l.Claim(ctx, time.Now().UTC().Add(ytCommentHoldGrace+2*time.Minute), op.ID); err != nil {
		t.Fatal(err)
	}
	got, err := l.ByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ytwrite.StateRejected || got.Reason != ytwrite.ReasonAmbiguousIssue || got.Attempts != 0 {
		t.Fatalf("held op past the grace %+v", got)
	}
}

// The ESC's sequence (PR #335 round 3), under hk 4318's design: comment op 2
// is queued behind issue op 1, and the task moves. Op 1 stays the task's one
// issue op (same id, same key), so op 2 stays behind it in plain lane order,
// is not refused or rejected, and is delivered to the issue op 1 creates in
// the task's new project, whose binding names op 1, the op the previous
// release also reads a queued comment's target from.
func TestYTCommentQueuedFollowsItsIssueAcrossAMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_moved_issue")
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
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].ID != create.ID || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("issue ops after the move %+v", ops)
	}
	if out := ytiClaim(t, s, op); out != ytwrite.ClaimBlocked {
		t.Fatalf("claim of the comment before the create = %s", out)
	}

	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	got := ytqDrive(t, s, ytcProtocol(t, s, yt, &offset), op.ID)
	b, found, err := s.YTTaskBinding(ctx, x.ID)
	if err != nil || !found || b.OpID != create.ID || b.ProjectID != "0-9" {
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

// A comment made after the move is queued behind the same op, not refused
// ErrYTCommentUnsendable; the comments keep their order and both reach the
// issue in the new project.
func TestYTCommentQueuedKeepsOrderAcrossAMove(t *testing.T) {
	s, x, create := ytqSetup(t, "ytq_order_move")
	ctx := t.Context()
	ytrProject(t, s, "ytq-other", "0-9")
	first, err := s.CreateTaskComment(ctx, x.ID, "worker", "first, before the move")
	if err != nil {
		t.Fatal(err)
	}
	ytrMove(t, s, x.ID, "ytq-other")
	second, err := s.CreateTaskComment(ctx, x.ID, "worker", "second, after the move")
	if err != nil {
		t.Fatalf("comment after the move: %v", err)
	}
	op1, _ := ytcOpFor(t, s, first.ID)
	op2, _ := ytcOpFor(t, s, second.ID)
	if !(create.ID < op1.ID && op1.ID < op2.ID) || op2.YTTargetID != "" {
		t.Fatalf("lane order: create %d, comments %+v %+v", create.ID, op1, op2)
	}
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	ytqDrive(t, s, p, op1.ID)
	got := ytqDrive(t, s, p, op2.ID)
	if got.State != ytwrite.StateApplied {
		t.Fatalf("second comment op %+v", got)
	}
	b, _, _ := s.YTTaskBinding(ctx, x.ID)
	comments := yt.Comments(b.IssueID)
	if b.ProjectID != "0-9" || len(comments) != 2 {
		t.Fatalf("binding %+v, comments %+v", b, comments)
	}
	if !strings.Contains(comments[0].Text, "first, before the move") || !strings.Contains(comments[1].Text, "second, after the move") {
		t.Fatalf("comments out of order: %+v", comments)
	}
}

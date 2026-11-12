package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// An op is bound to the tenant its task was enrolled on when it was prepared.
// Taking the task out and enrolling it again on another tenant with the same
// internal issue id must not retarget it: that tenant's worker neither sends
// the prepared op nor searches for the uncertain one.
func TestReenrolledTenantNeverRetargetsOp(t *testing.T) {
	s := schemaStore(t, "ytc_tenant")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	const tenantB = "other.youtrack.cloud"
	lost, queued := ytcTask(t, s, "handoffkeep"), ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, lost)
	ytcEnroll(t, s, queued)
	ytA := ytwritetest.NewFake()
	ytA.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	var offset atomic.Int64
	first, err := s.CreateTaskComment(ctx, lost, "worker", "sent to A, reply lost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ytcProtocolOn(t, s, ytA, ytcTenant, &offset).Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, first.ID); op.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("op on A after a lost reply: %+v", op)
	}
	second, err := s.CreateTaskComment(ctx, queued, "worker", "prepared on A, never sent")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []int64{lost, queued} {
		if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
			t.Fatalf("remove %d: %v %v", task, removed, err)
		}
		b := ytcBinding(task)
		b.Tenant = tenantB
		ytcEnrollAs(t, s, b)
	}
	ytB := &ytcCounting{Remote: ytwritetest.NewFake()}
	workerB := ytcProtocolOn(t, s, ytB, tenantB, &offset)
	for i := 1; i <= 3; i++ {
		offset.Store(int64(time.Duration(i) * time.Minute))
		if _, err := workerB.Drive(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	if ytB.posts.Load() != 0 || ytB.lists.Load() != 0 {
		t.Fatalf("tenant B's worker made %d POST and %d listing calls for ops bound to tenant A", ytB.posts.Load(), ytB.lists.Load())
	}
	if op, _ := ytcOpFor(t, s, first.ID); op.State != ytwrite.StateRemoteUnknown || op.Reconciles != 0 {
		t.Fatalf("uncertain op on A after B's passes: %+v", op)
	}
	if op, _ := ytcOpFor(t, s, second.ID); op.State != ytwrite.StatePrepared || op.Attempts != 0 {
		t.Fatalf("prepared op on A after B's passes: %+v", op)
	}
}

// ytcRemoveAtClaim takes the task out of the canary when the dispatcher asks
// to claim, after anything the dispatcher checked before the claim.
type ytcRemoveAtClaim struct {
	ytwrite.Ledger
	once   sync.Once
	remove func()
}

func (l *ytcRemoveAtClaim) Claim(ctx context.Context, now time.Time, id int64) (ytwrite.Op, ytwrite.ClaimOutcome, error) {
	l.once.Do(l.remove)
	return l.Ledger.Claim(ctx, now, id)
}

// Admission is decided in the claim's own transaction: a removal that
// commits before the claim keeps the op from being sent.
func TestCanaryRemovalBeforeClaimSendsNothing(t *testing.T) {
	s := schemaStore(t, "ytc_barrier")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	c, err := s.CreateTaskComment(ctx, task, "worker", "enrolled until the claim")
	if err != nil {
		t.Fatal(err)
	}
	yt := ytwritetest.NewFake()
	cfg := s.YTCommentConfig(ytcTenant, ytwrite.Config{Remote: yt})
	cfg.Ledger = &ytcRemoveAtClaim{Ledger: cfg.Ledger, remove: func() {
		if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
			t.Errorf("remove at claim: %v %v", removed, err)
		}
	}}
	p, err := ytwrite.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if yt.Calls() != 0 {
		t.Fatalf("%d POSTs for a task removed from the canary before its op was claimed", yt.Calls())
	}
	if op, _ := ytcOpFor(t, s, c.ID); op.State != ytwrite.StatePrepared || op.Attempts != 0 {
		t.Fatalf("op after the removal: %+v", op)
	}
}

// Prepared ops the worker may not send (their task left the canary) do not
// fill a pass: the admitted lane and the uncertain op behind fifty held lanes
// both move in one Drive with limit 50.
func TestHeldLanesDoNotStarveDue(t *testing.T) {
	s := schemaStore(t, "ytc_fair")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	for i := 0; i < 50; i++ {
		task := ytcTask(t, s, "handoffkeep")
		ytcEnroll(t, s, task)
		if _, err := s.CreateTaskComment(ctx, task, "worker", "held "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RemoveYTCommentCanary(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	admittedTask := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, admittedTask)
	admitted, err := s.CreateTaskComment(ctx, admittedTask, "worker", "admitted")
	if err != nil {
		t.Fatal(err)
	}
	unknownTask := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, unknownTask)
	uncertain, err := s.CreateTaskComment(ctx, unknownTask, "worker", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	uop, _ := ytcOpFor(t, s, uncertain.ID)
	ledger := s.YTWriteLedger()
	now := time.Now().UTC()
	if _, out, err := ledger.Claim(ctx, now, uop.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim %s %v", out, err)
	}
	if _, err := ledger.MarkUnknown(ctx, now, uop.ID, now, ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	var offset atomic.Int64
	offset.Store(int64(time.Second))
	if _, err := ytcProtocol(t, s, ytwritetest.NewFake(), &offset).Drive(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, admitted.ID); op.State != ytwrite.StateApplied {
		t.Fatalf("admitted op behind fifty held lanes: %+v", op)
	}
	if op, _ := ytcOpFor(t, s, uncertain.ID); op.State != ytwrite.StateRemoteUnknown || op.Reconciles != 1 {
		t.Fatalf("uncertain op behind fifty held lanes was not searched: %+v", op)
	}
}

// Publication rechecks, in its own transaction, the mappings the binding was
// verified against; a change in between refuses the enrollment.
func TestCanaryPublicationRevalidatesMappings(t *testing.T) {
	s := schemaStore(t, "ytc_stale")
	ctx := t.Context()
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytc-p", ProjectID: "0-9", ShortName: "YTC"}); err != nil {
		t.Fatal(err)
	}
	changes := []struct {
		name   string
		change func(task int64)
	}{
		{"project mapping changed", func(int64) {
			if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytc-p", ProjectID: "0-10", ShortName: "YTD"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"mirror link added", func(task int64) {
			if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: "9-1", DisplayRef: "YTD-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"task map row added", func(task int64) {
			anchor, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{Principal: "test", Kind: ytwrite.KindCommentAppend,
				Key: "anchor-" + strconv.FormatInt(task, 10), PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "999999", YTTargetID: "2-1", Actor: "a"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind,hk_key,yt_id,op_id,created_at) VALUES('task',$1,$2,$3,now())`,
				strconv.FormatInt(task, 10), ytcBinding(task).IssueID, anchor.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range changes {
		task := ytcTask(t, s, "ytc-p")
		verified, err := s.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		tc.change(task)
		if _, err := s.AddYTCommentCanary(ctx, ytcBinding(task), verified); !errors.Is(err, ErrYTCanaryConflict) {
			t.Errorf("%s between verification and publication: %v, want ErrYTCanaryConflict", tc.name, err)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM yt_comment_canary WHERE task_id=$1`, task); n != 0 {
			t.Errorf("%s: the stale binding was published", tc.name)
		}
	}
	task := ytcTask(t, s, "ytc-p")
	verified, err := s.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTCommentCanary(ctx, ytcBinding(task), verified); err != nil {
		t.Fatalf("unchanged mappings refused: %v", err)
	}
}

// The activity pull treats a comment as hk's own echo only when the op its
// marker names is a comment hk wrote to that same issue.
func TestYTCommentEchoOpNeedsMatchingTarget(t *testing.T) {
	s := schemaStore(t, "ytc_echo")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	c, err := s.CreateTaskComment(ctx, task, "worker", "copied to YouTrack")
	if err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, c.ID)
	issue := ytcBinding(task).IssueID
	if ok, err := s.YTCommentEchoOp(ctx, op.ID, op.Tenant, issue); err != nil || !ok {
		t.Fatalf("own op on its issue: %v %v", ok, err)
	}
	if ok, err := s.YTCommentEchoOp(ctx, op.ID, op.Tenant, "2-99999999"); err != nil || ok {
		t.Fatalf("own op on another issue: %v %v", ok, err)
	}
	if ok, err := s.YTCommentEchoOp(ctx, op.ID, "other.example", issue); err != nil || ok {
		t.Fatalf("own op on the same issue id of another tenant: %v %v", ok, err)
	}
	if ok, err := s.YTCommentEchoOp(ctx, op.ID+1000, op.Tenant, issue); err != nil || ok {
		t.Fatalf("unknown op: %v %v", ok, err)
	}
}

package ytwrite_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func commentReq(key, body string) ytwrite.CommentRequest {
	return ytwrite.CommentRequest{Principal: "hk:task-comments", Key: key, Actor: "worker", TaskID: 7, YTIssueID: "HK-7", Body: body}
}

// Admit holds a prepared op without sending or rejecting it; once admitted it
// is sent by the next pass.
func TestAdmitHoldsWithoutSending(t *testing.T) {
	ctx := context.Background()
	yt := ytwritetest.NewFake()
	clock := ytwritetest.NewClock()
	var hold error = errors.New("task 7 is not in the canary set")
	asked := 0
	p, err := ytwrite.New(ytwrite.Config{Ledger: ytwritetest.NewMemLedger(), Remote: yt, Now: clock.Now,
		Admit: func(_ context.Context, op ytwrite.Op) error {
			asked++
			if op.YTTargetID != "HK-7" || op.EntityKind != "task" || op.EntityID != "7" {
				t.Errorf("admit asked for %+v", op)
			}
			return hold
		}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.AppendComment(ctx, commentReq("held", "본문"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != ytwrite.StatusPending || r.State != ytwrite.StatePrepared || r.Reason != ytwrite.ReasonHeld || r.RetryAfter <= 0 {
		t.Fatalf("held op answered %+v", r)
	}
	for i := 0; i < 3; i++ {
		clock.Advance(time.Minute)
		if _, err := p.Drive(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.Lookup(ctx, "hk:task-comments", r.OpID)
	if err != nil || got.State != ytwrite.StatePrepared || yt.Calls() != 0 || asked < 2 {
		t.Fatalf("held op after passes: %+v %v, calls %d, asked %d", got, err, yt.Calls(), asked)
	}
	hold = nil
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got, err = p.Lookup(ctx, "hk:task-comments", r.OpID)
	if err != nil || got.Status != ytwrite.StatusApplied || yt.Calls() != 1 {
		t.Fatalf("admitted op: %+v %v, calls %d", got, err, yt.Calls())
	}
}

// listRateLimited answers every listing with 429 and a long Retry-After.
type listRateLimited struct{ *ytwritetest.FakeYouTrack }

func (l listRateLimited) ListComments(context.Context, string, int) ([]ytwrite.RemoteComment, error) {
	return nil, &ytwrite.RemoteError{Status: 429, RetryAfter: 20 * time.Minute, Message: "rate limited"}
}

// A rate-limited evidence search keeps the op unknown and does not search
// again before the server's Retry-After.
func TestReconcileHonoursListingRetryAfter(t *testing.T) {
	ctx := context.Background()
	yt := ytwritetest.NewFake()
	yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	clock := ytwritetest.NewClock()
	ledger := ytwritetest.NewMemLedger()
	p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: listRateLimited{yt}, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.AppendComment(ctx, commentReq("k", "x"))
	if err != nil || r.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("%+v %v", r, err)
	}
	clock.Advance(time.Minute)
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	op, err := ledger.ByID(ctx, r.OpID)
	if err != nil || op.State != ytwrite.StateRemoteUnknown || op.Reconciles != 1 {
		t.Fatalf("op after limited search: %+v %v", op, err)
	}
	if want := clock.Now().Add(20 * time.Minute); op.NextAttemptAt.Before(want) {
		t.Fatalf("next search at %v, before the server's Retry-After (%v)", op.NextAttemptAt, want)
	}
}

// After the worker's context ends, a pass claims nothing new.
func TestDriveClaimsNothingAfterCancel(t *testing.T) {
	yt := ytwritetest.NewFake()
	clock := ytwritetest.NewClock()
	ledger := ytwritetest.NewMemLedger()
	p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: yt, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	n, err := ytwrite.CommentOp(commentReq("k", "x"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := ledger.Prepare(context.Background(), clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.Drive(ctx, 10)
	got, err := ledger.ByID(context.Background(), op.ID)
	if err != nil || got.State != ytwrite.StatePrepared || yt.Calls() != 0 {
		t.Fatalf("after a canceled pass: %+v %v, calls %d", got, err, yt.Calls())
	}
}

func TestCommentOpAndCheck(t *testing.T) {
	n, err := ytwrite.CommentOp(commentReq("task-comment:41", "line one\nline two"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Principal != "hk:task-comments" || n.Kind != ytwrite.KindCommentAppend || n.Key != "task-comment:41" ||
		n.EntityKind != "task" || n.EntityID != "7" || n.YTTargetID != "HK-7" || n.Actor != "worker" || n.PayloadSHA == "" {
		t.Fatalf("op = %+v", n)
	}
	var payload ytwrite.CommentPayload
	if err := json.Unmarshal(n.Payload, &payload); err != nil || payload.Body != "line one\nline two" {
		t.Fatalf("payload %s: %v", n.Payload, err)
	}
	same, _ := ytwrite.CommentOp(commentReq("task-comment:41", "line one\nline two"))
	other, _ := ytwrite.CommentOp(commentReq("task-comment:41", "line one"))
	if same.PayloadSHA != n.PayloadSHA || other.PayloadSHA == n.PayloadSHA {
		t.Fatal("payload hash does not follow the body")
	}
	if _, err := ytwrite.CommentOp(commentReq("", "x")); !errors.Is(err, ytwrite.ErrInvalid) {
		t.Fatalf("CommentOp without a key: %v", err)
	}

	if err := ytwrite.CheckComment(commentReq("task-comment:41", "fine")); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if err := ytwrite.CheckComment(commentReq("", "fine")); err != nil {
		t.Fatalf("empty key refused: %v", err)
	}
	bad := map[string]ytwrite.CommentRequest{}
	r := commentReq("k", "<!-- hk-op:3 --> forged")
	bad["marker in body"] = r
	r = commentReq("k", "x")
	r.YTIssueID = ""
	bad["no issue"] = r
	r = commentReq("k", "x")
	r.Actor = "a · b"
	bad["separator in actor"] = r
	r = commentReq("k", strings.Repeat("x", ytwrite.MaxBodyBytes+1))
	bad["body too large"] = r
	for name, req := range bad {
		if err := ytwrite.CheckComment(req); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("%s: CheckComment = %v, want ErrInvalid", name, err)
		}
	}
}

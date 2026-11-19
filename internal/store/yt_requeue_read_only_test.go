package store

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// hk 5506: the requeue of the ops YouTrack's read-only answer rejected before
// hk 5494 (#521). ytrroRecorded is the last_error writeFailed recorded for
// such a comment POST (RemoteError.Error over APIError.Error).
const ytrroRecorded = "youtrack answered 418: youtrack POST /api/issues/2-900/comments: 418 invalid_state: The application is currently in read-only mode. Please try again later."

// ytrroRejected is an op as the rejected-before-hk-5494 row holds it.
func ytrroRejected(kind string, last string) ytwrite.Op {
	return ytwrite.Op{ID: 7, Kind: kind, State: ytwrite.StateRejected, Reason: "http_418", LastError: last,
		Payload: []byte(`{"body":"x"}`), Attempts: 1}
}

// TestYTRequeueRefusal pins the row-level eligibility (hk 5506): every
// condition that makes an op not eligible names its reason, and only a
// rejected read-only 418 of a single-write kind with a payload and no
// reconcile passes.
func TestYTRequeueRefusal(t *testing.T) {
	cases := []struct {
		name string
		op   ytwrite.Op
		want string
	}{
		{"eligible comment", ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded), ""},
		{"eligible article create", ytrroRejected(ytwrite.KindArticleCreate, ytrroRecorded), ""},
		{"not rejected", func() ytwrite.Op {
			o := ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded)
			o.State = ytwrite.StateApplied
			return o
		}(), ytRequeueNotRejected},
		{"other 418 code", ytrroRejected(ytwrite.KindCommentAppend, "youtrack answered 418: youtrack POST /x: 418 invalid_request: nope"), ytRequeueNotReadOnly},
		{"read-only text, other reason", func() ytwrite.Op {
			o := ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded)
			o.Reason = "http_400"
			return o
		}(), ytRequeueNotReadOnly},
		{"planned kind", ytrroRejected(ytwrite.KindIssueLinks, ytrroRecorded), ytRequeueKindRefused},
		{"issue move", ytrroRejected(ytwrite.KindIssueMove, ytrroRecorded), ytRequeueKindRefused},
		{"redacted payload", func() ytwrite.Op {
			o := ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded)
			o.Payload = nil
			return o
		}(), ytRequeuePayloadGone},
		{"reconciled", func() ytwrite.Op {
			o := ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded)
			o.Reconciles = 1
			return o
		}(), ytRequeueReconciled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ytRequeueRefusal(c.op); got != c.want {
				t.Fatalf("ytRequeueRefusal = %q, want %q", got, c.want)
			}
		})
	}
}

// TestYTRequeueLinkScope pins which ops must still own their task comment's
// link (hk 5506): task-comment ops only. A document copy (hk:doc-comments) has
// no link row, so requiring one would make it unrequeueable.
func TestYTRequeueLinkScope(t *testing.T) {
	task := ytrroRejected(ytwrite.KindCommentAppend, ytrroRecorded)
	task.Principal = YTCommentPrincipal
	doc := task
	doc.Principal = ytDocCommentPrincipal
	article := ytrroRejected(ytwrite.KindArticleCreate, ytrroRecorded)
	if !ytRequeueLinkRequired(task) {
		t.Fatal("a task comment must keep its link")
	}
	if ytRequeueLinkRequired(doc) || ytRequeueLinkRequired(article) {
		t.Fatal("a document copy or an article is not linked to a task comment")
	}
}

// TestYTRequeueByLength refuses an operator name the history row cannot hold,
// before any op is read.
func TestYTRequeueByLength(t *testing.T) {
	s := &Store{}
	long := strings.Repeat("a", YTRequeueByMax+1)
	if _, err := s.RequeueYTReadOnlyOps(t.Context(), time.Now(), YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{1}, By: long, Apply: true}); err == nil {
		t.Fatal("a name longer than the history row accepts was taken")
	}
}

// TestYTRequeueOpsSelection pins the ids a pass considers: named ids in order
// without repeats, and a positive-id check.
func TestYTRequeueOpsSelection(t *testing.T) {
	s := &Store{}
	ids, next, err := s.selectYTRequeueOps(t.Context(), YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{9, 3, 9, 5}})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil || len(ids) != 3 || ids[0] != 9 || ids[1] != 3 || ids[2] != 5 {
		t.Fatalf("ids %v next %v", ids, next)
	}
	if _, _, err := s.selectYTRequeueOps(t.Context(), YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{0}}); err == nil {
		t.Fatal("a non-positive op id was accepted")
	}
}

// TestYTRequeueReadOnlyFlow is the store-level flow (hk 5506, CI with
// PostgreSQL): a comment op rejected with the recorded read-only answer is
// listed by a dry run that writes nothing, requeued once with its history
// row, left alone by a second pass, and sent exactly once by the dispatcher.
// An op rejected with another 418 code stays rejected.
func TestYTRequeueReadOnlyFlow(t *testing.T) {
	s, x, create := ytqSetup(t, "ytrro_flow")
	ctx := t.Context()
	now := time.Now().UTC()
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	// The issue_create applies first, so the comment is admitted by the
	// binding it needs; the comment itself is then rejected as the old code
	// rejected it.
	ytqDrive(t, s, p, create.ID)

	c, err := s.CreateTaskComment(ctx, x.ID, "worker", "rejected in the read-only window")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok {
		t.Fatal("no comment op")
	}
	if _, err := s.YTWriteLedger().Reject(ctx, now, op.ID, "http_418", ytrroRecorded); err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateTaskComment(ctx, x.ID, "worker", "rejected by another 418")
	if err != nil {
		t.Fatal(err)
	}
	other, ok := ytcOpFor(t, s, c2.ID)
	if !ok {
		t.Fatal("no second comment op")
	}
	if _, err := s.YTWriteLedger().Reject(ctx, now, other.ID, "http_418", "youtrack answered 418: youtrack POST /x: 418 invalid_request: nope"); err != nil {
		t.Fatal(err)
	}

	dry, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{op.ID, other.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Ops) != 2 || dry.Ops[0].Outcome != YTRequeueWouldRequeue || dry.Ops[1].Outcome != YTRequeueNotEligible || dry.Ops[1].Reason != ytRequeueNotReadOnly {
		t.Fatalf("dry run %+v", dry.Ops)
	}
	if got, _ := s.YTWriteLedger().ByID(ctx, op.ID); got.State != ytwrite.StateRejected || ytrroCount(t, s, op.ID) != 0 {
		t.Fatalf("dry run wrote: op %+v, history rows %d", got, ytrroCount(t, s, op.ID))
	}

	// An op of another tenant is refused, and nothing is written for it.
	foreign, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: "other.youtrack.cloud", OpIDs: []int64{op.ID}, By: "operator", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign.Ops) != 1 || foreign.Ops[0].Outcome != YTRequeueNotEligible || foreign.Ops[0].Reason != ytRequeueOtherTenant {
		t.Fatalf("other tenant %+v", foreign.Ops)
	}
	if got, _ := s.YTWriteLedger().ByID(ctx, op.ID); got.State != ytwrite.StateRejected || ytrroCount(t, s, op.ID) != 0 {
		t.Fatalf("other tenant wrote: op %+v, history rows %d", got, ytrroCount(t, s, op.ID))
	}

	if _, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{op.ID}, Apply: true}); err == nil {
		t.Fatal("apply without --by was accepted")
	}
	applied, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{op.ID}, By: "operator", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Ops) != 1 || applied.Ops[0].Outcome != YTRequeueRequeued {
		t.Fatalf("apply %+v", applied.Ops)
	}
	got, err := s.YTWriteLedger().ByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ytwrite.StatePrepared || got.Attempts != 0 || got.Reason != "" || got.LastError != "" || !got.ResolvedAt.IsZero() || got.Key != op.Key || string(got.Payload) != string(op.Payload) {
		t.Fatalf("requeued op %+v, want the fresh prepared shape of %+v", got, op)
	}
	if n := ytrroCount(t, s, op.ID); n != 1 {
		t.Fatalf("history rows %d, want 1", n)
	}

	again, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{op.ID}, By: "operator", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Ops) != 1 || again.Ops[0].Outcome != YTRequeueNotEligible || again.Ops[0].Reason != ytRequeueNotRejected || ytrroCount(t, s, op.ID) != 1 {
		t.Fatalf("second apply %+v, history rows %d", again.Ops, ytrroCount(t, s, op.ID))
	}

	// The dispatcher sends the requeued op once, with the same key and plan.
	ytqDrive(t, s, p, op.ID)
	b, found, err := s.YTTaskBinding(ctx, x.ID)
	if err != nil || !found {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	if comments := yt.Comments(b.IssueID); len(comments) != 1 {
		t.Fatalf("issue comments %+v, want exactly one", comments)
	}
}

// ytrroCount is the number of history rows an op has.
func ytrroCount(t *testing.T, s *Store, opID int64) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM yt_write_op_requeues WHERE op_id=$1`, opID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

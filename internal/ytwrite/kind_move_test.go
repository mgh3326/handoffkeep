package ytwrite_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const moveTask = int64(4055)

// prepareMove stores a move of issue to project in the ledger, under key.
func prepareMove(t *testing.T, e *kindEnv, issue, project, key string) ytwrite.Op {
	t.Helper()
	n, err := ytwrite.MoveOp(ytwrite.MoveRequest{Principal: "hk:task-fields", Actor: "hk:task-fields", Tenant: "tenant.youtrack.cloud",
		IssueID: issue, ProjectID: project, TaskID: moveTask})
	if err != nil {
		t.Fatal(err)
	}
	n.Key = key
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestMoveOpIdentity(t *testing.T) {
	req := ytwrite.MoveRequest{Principal: "hk:task-fields", Actor: "hk:task-fields", Tenant: "tenant.youtrack.cloud",
		IssueID: "2-1", ProjectID: "0-9", TaskID: moveTask}
	a, err := ytwrite.MoveOp(req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ytwrite.KindIssueMove || a.EntityKind != "task" || a.EntityID != "4055" || a.YTTargetID != "2-1" || a.Key != "" {
		t.Fatalf("op %+v", a)
	}
	if project, err := ytwrite.IssueMoveProject(ytwrite.Op{ID: 1, EntityKind: "task", YTTargetID: a.YTTargetID, Payload: a.Payload}); err != nil || project != "0-9" {
		t.Fatalf("target project %q %v", project, err)
	}
	same, _ := ytwrite.MoveOp(req)
	if same.PayloadSHA != a.PayloadSHA {
		t.Fatal("the same move hashes differently")
	}
	other, _ := ytwrite.MoveOp(ytwrite.MoveRequest{Principal: req.Principal, Actor: req.Actor, Tenant: req.Tenant, IssueID: "2-1", ProjectID: "0-10", TaskID: moveTask})
	if other.PayloadSHA == a.PayloadSHA {
		t.Fatal("a move to another project hashes the same")
	}
	for _, bad := range []ytwrite.MoveRequest{
		{Principal: "hk:task-fields", Actor: "hk:task-fields", Tenant: "t", IssueID: "", ProjectID: "0-9", TaskID: moveTask},
		{Principal: "hk:task-fields", Actor: "hk:task-fields", Tenant: "t", IssueID: "2-1", ProjectID: "", TaskID: moveTask},
		{Principal: "hk:task-fields", Actor: "a · b", Tenant: "t", IssueID: "2-1", ProjectID: "0-9", TaskID: moveTask},
		{Principal: "hk:task-fields", Actor: "a", Tenant: "", IssueID: "2-1", ProjectID: "0-9", TaskID: moveTask},
		{Principal: "hk:task-fields", Actor: "a", Tenant: "t", IssueID: "2-1", ProjectID: "0-9", TaskID: 0},
	} {
		if _, err := ytwrite.MoveOp(bad); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("MoveOp(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
	for _, bad := range []ytwrite.Op{
		{ID: 2, EntityKind: "task", YTTargetID: "2-1", Payload: []byte(`{"project_id":""}`)},
		{ID: 3, EntityKind: "task", YTTargetID: "2-1", Payload: []byte(`not json`)},
		{ID: 4, EntityKind: "comment", YTTargetID: "2-1", Payload: []byte(`{"project_id":"0-9"}`)},
	} {
		if _, err := ytwrite.IssueMoveProject(bad); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("IssueMoveProject(op %d) = %v, want ErrInvalid", bad.ID, err)
		}
	}
	var p ytwrite.IssueMovePayload
	if err := json.Unmarshal(a.Payload, &p); err != nil || p.ProjectID != "0-9" {
		t.Fatalf("payload %s: %v", a.Payload, err)
	}
}

func TestIssueMoveSendMovesTheIssue(t *testing.T) {
	e := newKindEnv(t)
	issue := e.yt.InjectIssue(issueProject, moveTask, "#4055 move me", "description")
	op := prepareMove(t, e, issue, "0-9", "k1")
	e.drive()
	if got := e.want(op.ID, ytwrite.StateApplied, ""); got.YTResultID != issue {
		t.Fatalf("op %+v", got)
	}
	if project, ok := e.yt.ProjectOf(issue); !ok || project != "0-9" {
		t.Fatalf("issue is in %q %v, want 0-9", project, ok)
	}
	if moves := e.yt.Moves(); len(moves) != 1 || moves[0] != (ytwritetest.FakeMove{IssueID: issue, ProjectID: "0-9"}) {
		t.Fatalf("moves %+v", moves)
	}
}

func TestIssueMoveEvidence(t *testing.T) {
	t.Run("moved_lost_reply_applied_by_project", func(t *testing.T) {
		e := newKindEnv(t)
		issue := e.yt.InjectIssue(issueProject, moveTask, "#4055 x", "d")
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := prepareMove(t, e, issue, "0-9", "k1")
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		if got := e.want(op.ID, ytwrite.StateApplied, ""); got.YTResultID != issue || e.yt.Calls() != 1 || e.yt.Reads() != 1 {
			t.Fatalf("op %+v, calls %d, reads %d", got, e.yt.Calls(), e.yt.Reads())
		}
	})
	t.Run("not_moved_stays_unknown", func(t *testing.T) {
		e := newKindEnv(t)
		issue := e.yt.InjectIssue(issueProject, moveTask, "#4055 x", "d")
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.ServerError})
		op := prepareMove(t, e, issue, "0-9", "k1")
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		e.clock.Advance(time.Hour)
		e.drive()
		// The issue is still in its project: no evidence, and the op is not
		// sent again.
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
		if project, _ := e.yt.ProjectOf(issue); project != issueProject || e.yt.Calls() != 1 || e.yt.Reads() != 1 {
			t.Fatalf("project %q, calls %d, reads %d", project, e.yt.Calls(), e.yt.Reads())
		}
	})
	t.Run("moved_elsewhere_is_no_evidence", func(t *testing.T) {
		e := newKindEnv(t)
		issue := e.yt.InjectIssue(issueProject, moveTask, "#4055 x", "d")
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := prepareMove(t, e, issue, "0-9", "k1")
		e.drive()
		// Someone moved it to a third project: that is not this op's effect.
		if _, err := e.yt.MoveIssueToProject(context.Background(), issue, "0-11"); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
	})
}

// A remote that cannot move issues refuses the op before it sends anything.
func TestIssueMoveNeedsAMoveRemote(t *testing.T) {
	e := newKindEnv(t)
	issue := e.yt.InjectIssue(issueProject, moveTask, "#4055 x", "d")
	p, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: plainRemote{e.yt}, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.p = p
	op := prepareMove(t, e, issue, "0-9", "k1")
	e.drive()
	got := e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind)
	if got.YTResultID != "" || len(e.yt.Moves()) != 0 {
		t.Fatalf("op %+v, moves %+v", got, e.yt.Moves())
	}
}

// plainRemote is the protocol's Remote and nothing more.
type plainRemote struct{ ytwrite.Remote }

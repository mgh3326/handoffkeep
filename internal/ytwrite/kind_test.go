package ytwrite_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Golden values computed at 39325a0 (before the kind registry): a comment op
// prepared before this release must keep its payload, payload hash and text.
const (
	goldenCommentText    = "작성: operator-desk · 2026-11-11T05:06:07Z · hk op 3864\n\n한글 본문\n- item `code`\n\n끝 · end\n\n<!-- hk-op:3864 -->"
	goldenCommentSHA     = "184c8503c6e8d7ddeff5a7c4dfc0f02a46f9b3be1f23a9207b5df4415265450b"
	goldenCommentPayload = "{\"body\":\"한글 본문\\n- item `code`\\n\\n끝 · end\"}"
)

func TestCommentRenderGolden(t *testing.T) {
	req := ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:41", Actor: "operator-desk", TaskID: 3864, YTIssueID: "2-3864", Tenant: "tenant.youtrack.cloud", Body: "한글 본문\n- item `code`\n\n끝 · end"}
	n, err := ytwrite.CommentOp(req)
	if err != nil {
		t.Fatal(err)
	}
	if n.PayloadSHA != goldenCommentSHA || string(n.Payload) != goldenCommentPayload {
		t.Fatalf("op payload %s sha %s, want %s sha %s", n.Payload, n.PayloadSHA, goldenCommentPayload, goldenCommentSHA)
	}
	op := ytwrite.Op{ID: 3864, Kind: n.Kind, Actor: n.Actor, Payload: n.Payload, CreatedAt: time.Date(2026, 11, 11, 5, 6, 7, 123456789, time.UTC)}
	p, err := ytwrite.New(ytwrite.Config{Ledger: ytwritetest.NewMemLedger(), Remote: ytwritetest.NewFake()})
	if err != nil {
		t.Fatal(err)
	}
	viaKind, err := p.Kinds[ytwrite.KindCommentAppend].Render(op)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := ytwrite.RenderComment(op)
	if err != nil {
		t.Fatal(err)
	}
	if viaKind != goldenCommentText || direct != goldenCommentText {
		t.Fatalf("rendered %q (kind) and %q (RenderComment), want %q", viaKind, direct, goldenCommentText)
	}
}

func TestRegisteredKinds(t *testing.T) {
	if got := ytwrite.RegisteredKinds(); len(got) != 1 || got[0] != ytwrite.KindCommentAppend {
		t.Fatalf("registered kinds %v", got)
	}
}

const kindNote = "test_note"

// noteKind is a test-only second kind: a note written as a comment in its own
// text form. It counts its calls so a test can tell the protocol dispatched
// to it and not to the comment kind.
type noteKind struct{ sends, searches atomic.Int32 }

func (*noteKind) Render(op ytwrite.Op) (string, error) {
	return "note " + string(op.Payload) + " " + ytwrite.OpMarker(op.ID), nil
}

func (k *noteKind) Send(ctx context.Context, remote ytwrite.Remote, op ytwrite.Op, text string) (string, error) {
	k.sends.Add(1)
	return remote.CreateComment(ctx, op.YTTargetID, text)
}

func (k *noteKind) Evidence(ctx context.Context, remote ytwrite.Remote, op ytwrite.Op, text string) (ytwrite.Evidence, error) {
	k.searches.Add(1)
	var ev ytwrite.Evidence
	cs, err := remote.ListComments(ctx, op.YTTargetID, 100)
	if err != nil {
		return ev, err
	}
	for _, c := range cs {
		if !strings.Contains(c.Text, ytwrite.OpMarker(op.ID)) {
			continue
		}
		switch {
		case c.Text != text:
			ev.Mismatched++
		case c.Deleted:
			ev.Deleted++
		default:
			ev.Matches = append(ev.Matches, c.ID)
		}
	}
	return ev, nil
}

type kindEnv struct {
	t      *testing.T
	yt     *ytwritetest.FakeYouTrack
	clock  *ytwritetest.Clock
	ledger *ytwritetest.MemLedger
	p      *ytwrite.Protocol
	note   *noteKind
}

func newKindEnv(t *testing.T) *kindEnv {
	t.Helper()
	e := &kindEnv{t: t, yt: ytwritetest.NewFake(), clock: ytwritetest.NewClock(), ledger: ytwritetest.NewMemLedger(), note: &noteKind{}}
	p, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: e.yt, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	p.Kinds[kindNote] = e.note
	e.p = p
	return e
}

// prepare stores an op of kind directly in the ledger (no public entry for
// a test kind) and returns its id.
func (e *kindEnv) prepare(kind, key string) int64 {
	e.t.Helper()
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), ytwrite.NewOp{
		Principal: "agent-a", Kind: kind, Key: key, PayloadSHA: "sha-" + key, Payload: []byte(`{"n":1}`),
		EntityKind: "task", EntityID: "3864", YTTargetID: "2-3864", Actor: "worker",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return op.ID
}

func (e *kindEnv) drive() {
	e.t.Helper()
	if _, err := e.p.Drive(context.Background(), 10); err != nil {
		e.t.Fatal(err)
	}
}

func (e *kindEnv) want(id int64, state ytwrite.State, reason string) ytwrite.Op {
	e.t.Helper()
	op, err := e.ledger.ByID(context.Background(), id)
	if err != nil || op.State != state || op.Reason != reason {
		e.t.Fatalf("op %d = %s/%q %v, want %s/%q", id, op.State, op.Reason, err, state, reason)
	}
	return op
}

// The generic send and reconcile paths dispatch to the op's kind and apply the
// shared evidence rule to what it reports.
func TestRegisteredKindUsesGenericPaths(t *testing.T) {
	t.Run("sent_applied", func(t *testing.T) {
		e := newKindEnv(t)
		id := e.prepare(kindNote, "k")
		e.drive()
		op := e.want(id, ytwrite.StateApplied, "")
		cs := e.yt.Comments("2-3864")
		if len(cs) != 1 || cs[0].ID != op.YTResultID || cs[0].Text != `note {"n":1} `+ytwrite.OpMarker(id) || e.note.sends.Load() != 1 {
			t.Fatalf("remote %+v, op %+v, sends %d", cs, op, e.note.sends.Load())
		}
	})
	t.Run("lost_reply_then_one_match_applied", func(t *testing.T) {
		e := newKindEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		id := e.prepare(kindNote, "k")
		e.drive()
		e.want(id, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		op := e.want(id, ytwrite.StateApplied, "")
		if op.YTResultID != e.yt.Comments("2-3864")[0].ID || e.note.searches.Load() != 1 || e.yt.Calls() != 1 {
			t.Fatalf("op %+v, searches %d, calls %d", op, e.note.searches.Load(), e.yt.Calls())
		}
	})
	t.Run("zero_matches_no_evidence", func(t *testing.T) {
		e := newKindEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.DelayCommit})
		id := e.prepare(kindNote, "k")
		e.drive()
		e.clock.Advance(time.Hour)
		e.drive()
		if op := e.want(id, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence); op.Reconciles != 1 || e.yt.Calls() != 1 {
			t.Fatalf("op %+v, calls %d", op, e.yt.Calls())
		}
	})
	t.Run("two_matches_multiple_matches", func(t *testing.T) {
		e := newKindEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		id := e.prepare(kindNote, "k")
		e.drive()
		e.yt.Duplicate("2-3864", e.yt.Comments("2-3864")[0].ID)
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(id, ytwrite.StateRemoteUnknown, ytwrite.ReasonMultipleMatches)
	})
	t.Run("send_4xx_rejected", func(t *testing.T) {
		e := newKindEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusNotFound})
		id := e.prepare(kindNote, "k")
		e.drive()
		e.want(id, ytwrite.StateRejected, "http_404")
	})
	t.Run("unregistered_kind_rejected", func(t *testing.T) {
		e := newKindEnv(t)
		id := e.prepare("not_registered", "k")
		e.drive()
		if op := e.want(id, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind); op.LastError != "not_registered" || e.yt.Calls() != 0 {
			t.Fatalf("op %+v, calls %d", op, e.yt.Calls())
		}
	})
}

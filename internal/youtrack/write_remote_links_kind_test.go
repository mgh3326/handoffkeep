package youtrack

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// The issue_links kind (hk 4638 S3) end to end over the real adapter and the
// fake's HTTP front end: the planned send, its I12 order on the wire, a
// person-made link left alone, and a partial write recovered by a new plan.
func TestIssueLinksKindOverHTTP(t *testing.T) {
	f := ytwritetest.NewFake()
	remote := serveLinks(t, f, nil)
	clock := ytwritetest.NewClock()
	ledger := ytwritetest.NewMemLedger()
	p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: remote, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seq := 0
	run := func(d ytwrite.IssueLinksDesired) ytwrite.Op {
		t.Helper()
		n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk", Tenant: "yt.example",
			IssueID: "2-10", TaskID: 10, Desired: d})
		if err != nil {
			t.Fatal(err)
		}
		seq++
		n.Key = "k" + strconv.Itoa(seq)
		op, _, err := ledger.Prepare(ctx, clock.Now(), n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Drive(ctx, 10); err != nil {
			t.Fatal(err)
		}
		got, err := ledger.ByID(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	mutations := func() []string {
		var out []string
		for _, r := range linkReqs(f) {
			if r[:3] != "GET" {
				out = append(out, r)
			}
		}
		return out
	}
	f.SeedIssueLink("2-10", ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: "2-99"})

	op := run(ytwrite.IssueLinksDesired{Parent: &ytwrite.IssueLinkTarget{TaskID: 30, IssueID: "2-30"},
		Related: []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}}})
	if op.State != ytwrite.StateApplied || op.YTResultID != "2-10" {
		t.Fatalf("first op = %s/%s %q", op.State, op.Reason, op.LastError)
	}
	want := []string{"POST /api/issues/2-10/links/105-3t/issues", "POST /api/issues/2-10/links/105-0/issues"}
	if got := mutations(); !slices.Equal(got, want) {
		t.Fatalf("first op requests = %q, want %q", got, want)
	}

	// Re-parent and drop the related edge: the old parent goes first, the
	// person-made relates link stays.
	mark := len(mutations())
	op = run(ytwrite.IssueLinksDesired{Parent: &ytwrite.IssueLinkTarget{TaskID: 31, IssueID: "2-31"},
		Related: []ytwrite.IssueLinkTarget{{TaskID: 21, IssueID: "2-21"}}})
	if op.State != ytwrite.StateApplied {
		t.Fatalf("second op = %s/%s %q", op.State, op.Reason, op.LastError)
	}
	want = []string{"DELETE /api/issues/2-10/links/105-3t/issues/2-30", "POST /api/issues/2-10/links/105-3t/issues",
		"POST /api/issues/2-10/links/105-0/issues", "DELETE /api/issues/2-10/links/105-0/issues/2-20"}
	if got := mutations()[mark:]; !slices.Equal(got, want) {
		t.Fatalf("second op requests = %q, want %q", got, want)
	}
	wantLinks := []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-31"}, {Kind: ytwrite.LinkRelates, Target: "2-21"}, {Kind: ytwrite.LinkRelates, Target: "2-99"}}
	if got := f.IssueLinks("2-10"); !slices.Equal(got, wantLinks) {
		t.Fatalf("links = %+v", got)
	}

	// A partial write over HTTP: the add lands, the remove answers 503.
	f.QueueLinkFaults(ytwritetest.LinkFault{Method: http.MethodDelete, Status: http.StatusServiceUnavailable, Count: 1})
	op = run(ytwrite.IssueLinksDesired{Parent: &ytwrite.IssueLinkTarget{TaskID: 31, IssueID: "2-31"},
		Related: []ytwrite.IssueLinkTarget{{TaskID: 22, IssueID: "2-22"}}})
	if op.State != ytwrite.StateRemoteUnknown || op.Reason != ytwrite.ReasonPartialWrite {
		t.Fatalf("partial op = %s/%s %q", op.State, op.Reason, op.LastError)
	}
	clock.Advance(time.Hour)
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, err = ledger.ByID(ctx, op.ID); err != nil || op.State != ytwrite.StateApplied {
		t.Fatalf("recovered op = %+v %v", op, err)
	}
	wantLinks = []ytwrite.IssueLink{{Kind: ytwrite.LinkSubtask, Target: "2-31"}, {Kind: ytwrite.LinkRelates, Target: "2-22"}, {Kind: ytwrite.LinkRelates, Target: "2-99"}}
	if got := f.IssueLinks("2-10"); !slices.Equal(got, wantLinks) {
		t.Fatalf("links after recovery = %+v", got)
	}
}

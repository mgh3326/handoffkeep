package ytwrite_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func TestIssueLinksSnapshotRemovesStaleParentAndRelations(t *testing.T) {
	e := newKindEnv(t)
	issue := "2-3864"
	want := ytwrite.IssueLinks{Parent: "2-parent", Related: []string{"2-related"}, Duplicates: []string{}}
	previous := ytwrite.IssueLinks{Parent: "2-old", Related: []string{"2-old-related"}, Duplicates: []string{"2-old-duplicate"}}
	e.yt.SetIssueLinks(issue, previous)
	request := func(key string, links ytwrite.IssueLinks) int64 {
		var owned ytwrite.IssueLinks
		if key == "links-one" {
			owned = previous
		}
		if key == "links-two" {
			owned = want
		}
		n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk:task-links", Tenant: "tenant.youtrack.cloud", IssueID: issue, TaskID: 3864, Previous: owned, Links: links})
		if err != nil {
			t.Fatal(err)
		}
		n.Key = key
		op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
		if err != nil {
			t.Fatal(err)
		}
		return op.ID
	}
	first := request("links-one", want)
	e.drive()
	e.want(first, ytwrite.StateApplied, "")
	got, err := e.yt.ReadIssueLinks(context.Background(), issue)
	if err != nil || got.Parent != want.Parent || len(got.Related) != 1 || got.Related[0] != "2-related" || len(got.Duplicates) != 0 {
		t.Fatalf("after link snapshot: %+v err=%v", got, err)
	}
	cleared := ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}
	second := request("links-two", cleared)
	e.drive()
	e.want(second, ytwrite.StateApplied, "")
	got, err = e.yt.ReadIssueLinks(context.Background(), issue)
	if err != nil || got.Parent != "" || len(got.Related) != 0 || len(got.Duplicates) != 0 {
		t.Fatalf("after removal snapshot: %+v err=%v", got, err)
	}
}

func TestIssueLinksRejectsSelfLink(t *testing.T) {
	_, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk:task-links", Tenant: "tenant.youtrack.cloud", IssueID: "2-1", TaskID: 1,
		Links: ytwrite.IssueLinks{Parent: "2-1"}})
	if err == nil {
		t.Fatal("self-link accepted")
	}
}

func TestIssueLinksDoesNotRemoveUnownedYouTrackLinks(t *testing.T) {
	e := newKindEnv(t)
	issue := "2-3864"
	human := ytwrite.IssueLinks{Related: []string{"2-human"}, Duplicates: []string{}}
	e.yt.SetIssueLinks(issue, human)
	n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk:task-links", Tenant: "tenant.youtrack.cloud", IssueID: issue, TaskID: 3864,
		Links: ytwrite.IssueLinks{Related: []string{"2-hk"}, Duplicates: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	n.Key = "human-link-conflict"
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnownedLink)
	got, err := e.yt.ReadIssueLinks(context.Background(), issue)
	if err != nil || len(got.Related) != 1 || got.Related[0] != "2-human" || len(e.yt.IssueLinkWrites()) != 0 {
		t.Fatalf("unowned link changed: got=%+v writes=%+v err=%v", got, e.yt.IssueLinkWrites(), err)
	}
}

func TestIssueLinksRecoveryReappliesOnlyRemainingChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial ytwrite.IssueLinks
		partial ytwrite.IssueLinks
		desired ytwrite.IssueLinks
		status  int
	}{
		{name: "remaining additions after 503", initial: emptyIssueLinks(), partial: ytwrite.IssueLinks{Related: []string{"2-a"}, Duplicates: []string{}}, desired: ytwrite.IssueLinks{Related: []string{"2-a", "2-b"}, Duplicates: []string{}}, status: http.StatusServiceUnavailable},
		{name: "remaining additions after 400", initial: emptyIssueLinks(), partial: ytwrite.IssueLinks{Related: []string{"2-a"}, Duplicates: []string{}}, desired: ytwrite.IssueLinks{Related: []string{"2-a", "2-b"}, Duplicates: []string{}}, status: http.StatusBadRequest},
		{name: "remaining deletions after 503", initial: ytwrite.IssueLinks{Related: []string{"2-a", "2-b"}, Duplicates: []string{}}, partial: ytwrite.IssueLinks{Related: []string{"2-b"}, Duplicates: []string{}}, desired: emptyIssueLinks(), status: http.StatusServiceUnavailable},
		{name: "remaining deletions after 400", initial: ytwrite.IssueLinks{Related: []string{"2-a", "2-b"}, Duplicates: []string{}}, partial: ytwrite.IssueLinks{Related: []string{"2-b"}, Duplicates: []string{}}, desired: emptyIssueLinks(), status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := ytwritetest.NewMemLedger()
			clock := ytwritetest.NewClock()
			base := ytwritetest.NewFake()
			issue := "2-3864"
			base.SetIssueLinks(issue, tc.initial)
			remote := &partialIssueLinksRemote{FakeYouTrack: base, partial: tc.partial, status: tc.status}
			p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: remote, Now: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk:task-links", Tenant: "tenant.youtrack.cloud", IssueID: issue, TaskID: 3864, Previous: tc.initial, Links: tc.desired})
			if err != nil {
				t.Fatal(err)
			}
			n.Key = "partial-links"
			op, _, err := ledger.Prepare(context.Background(), clock.Now(), n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if got, err := ledger.ByID(context.Background(), op.ID); err != nil || got.State != ytwrite.StateRemoteUnknown {
				t.Fatalf("first dispatch = %+v, %v; want remote_unknown", got, err)
			}
			clock.Advance(time.Minute)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if got, err := ledger.ByID(context.Background(), op.ID); err != nil || got.State != ytwrite.StateApplied {
				t.Fatalf("recovery = %+v, %v; want applied", got, err)
			}
			if remote.calls != 2 {
				t.Fatalf("link mutation calls %d, want initial attempt and read-diff recovery", remote.calls)
			}
			got, err := base.ReadIssueLinks(context.Background(), issue)
			if err != nil || len(got.Related) != len(tc.desired.Related) {
				t.Fatalf("final links %+v err=%v, want %+v", got, err, tc.desired)
			}
			for i := range got.Related {
				if got.Related[i] != tc.desired.Related[i] {
					t.Fatalf("final links %+v, want %+v", got, tc.desired)
				}
			}
		})
	}
}

type partialIssueLinksRemote struct {
	*ytwritetest.FakeYouTrack
	partial ytwrite.IssueLinks
	calls   int
	status  int
}

func (r *partialIssueLinksRemote) UpdateIssueLinks(ctx context.Context, issue string, snapshot ytwrite.IssueLinksSnapshot) (string, error) {
	r.calls++
	if r.calls == 1 {
		r.SetIssueLinks(issue, r.partial)
		return "", &ytwrite.PartialWriteError{Err: &ytwrite.RemoteError{Status: r.status, Message: "partial link mutation"}}
	}
	return r.FakeYouTrack.UpdateIssueLinks(ctx, issue, snapshot)
}

func emptyIssueLinks() ytwrite.IssueLinks {
	return ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}
}

var _ ytwrite.IssueLinksRemote = (*ytwritetest.FakeYouTrack)(nil)

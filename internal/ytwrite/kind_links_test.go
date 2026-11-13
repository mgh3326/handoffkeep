package ytwrite_test

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func TestIssueLinksSnapshotRemovesStaleParentAndRelations(t *testing.T) {
	e := newKindEnv(t)
	issue := "2-3864"
	e.yt.SetIssueLinks(issue, ytwrite.IssueLinks{Parent: "2-old", Related: []string{"2-old-related"}, Duplicates: []string{"2-old-duplicate"}})
	request := func(key string, links ytwrite.IssueLinks) int64 {
		n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk:task-links", Tenant: "tenant.youtrack.cloud", IssueID: issue, TaskID: 3864, Links: links})
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
	want := ytwrite.IssueLinks{Parent: "2-parent", Related: []string{"2-related"}, Duplicates: []string{}}
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

var _ ytwrite.IssueLinksRemote = (*ytwritetest.FakeYouTrack)(nil)

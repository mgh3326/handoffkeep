package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func TestYTLinksRelationArchiveRemovesYouTrackLink(t *testing.T) {
	s := schemaStore(t, "yt_links_relation")
	ytbMapped(t, s)
	source, target := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(source))
	ytbBind(t, s, ytbBinding(target))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	if _, err := s.AddTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil {
		t.Fatal(err)
	}
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(source), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	linksPass(t, s, remote)
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(source)); err != nil || len(got.Related) != 1 || got.Related[0] != ytfIssue(target) {
		t.Fatalf("added relation was not projected: %+v err=%v", got, err)
	}
	if changed, err := s.RemoveTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	linksPass(t, s, remote)
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(source)); err != nil || len(got.Related) != 0 {
		t.Fatalf("archived relation remains in YouTrack: %+v err=%v", got, err)
	}
}

func TestYTLinksClearedParentRemovesYouTrackLink(t *testing.T) {
	s := schemaStore(t, "yt_links_parent")
	ytbMapped(t, s)
	child, parent := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(child))
	ytbBind(t, s, ytbBinding(parent))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	if _, changed, err := s.SetTaskParent(t.Context(), child, &parent, "test"); err != nil || !changed {
		t.Fatalf("set parent changed=%t err=%v", changed, err)
	}
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(child), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	linksPass(t, s, remote)
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(child)); err != nil || got.Parent != ytfIssue(parent) {
		t.Fatalf("parent was not projected: %+v err=%v", got, err)
	}
	if _, changed, err := s.SetTaskParent(t.Context(), child, nil, "test"); err != nil || !changed {
		t.Fatalf("clear parent changed=%t err=%v", changed, err)
	}
	linksPass(t, s, remote)
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(child)); err != nil || got.Parent != "" {
		t.Fatalf("cleared parent remains in YouTrack: %+v err=%v", got, err)
	}
}

func linksPass(t *testing.T, s *Store, remote *ytwritetest.FakeYouTrack) {
	t.Helper()
	ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer])
	p, err := ytwrite.New(ytwrite.Config{Ledger: s.YTTenantLedger(ytcTenant), Remote: remote, Now: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
}

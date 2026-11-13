package store

import (
	"context"
	"fmt"
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

func TestYTLinksHigherEndpointKeepsNormalizedRelatedEdge(t *testing.T) {
	s := schemaStore(t, "yt_links_higher")
	ytbMapped(t, s)
	lower, higher := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(lower))
	ytbBind(t, s, ytbBinding(higher))
	if _, err := s.AddTaskRelation(t.Context(), higher, RelationRelated, lower, "test"); err != nil {
		t.Fatal(err)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := prepareYTLinksTx(t.Context(), tx, time.Now().UTC(), ytcTenant, higher)
	if err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if outcome != ytLinksPrepared {
		_ = tx.Rollback(t.Context())
		t.Fatalf("higher endpoint outcome %s, want prepared", outcome)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := s.pool.QueryRow(t.Context(), `SELECT payload FROM yt_write_ops WHERE principal=$1 AND entity_id=$2`, YTLinksPrincipal, fmt.Sprint(higher)).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ytwrite.DecodeIssueLinksSnapshot(payload)
	if err != nil || len(snapshot.Desired.Related) != 1 || snapshot.Desired.Related[0] != ytfIssue(lower) {
		t.Fatalf("higher endpoint snapshot %+v err=%v", snapshot, err)
	}
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(lower), ytwrite.IssueLinks{Related: []string{ytfIssue(higher)}, Duplicates: []string{}})
	remote.SetIssueLinks(ytfIssue(higher), ytwrite.IssueLinks{Related: []string{ytfIssue(lower)}, Duplicates: []string{}})
	p, err := ytwrite.New(ytwrite.Config{Ledger: s.YTTenantLedger(ytcTenant), Remote: remote, Now: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(lower)); err != nil || len(got.Related) != 1 || got.Related[0] != ytfIssue(higher) {
		t.Fatalf("lower endpoint relation was erased: %+v err=%v", got, err)
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

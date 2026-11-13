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

func TestYTLinksQueuedAddThenRemovalUsesAppliedOwnership(t *testing.T) {
	s := schemaStore(t, "yt_links_queued_remove")
	ytbMapped(t, s)
	source, target := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(source))
	ytbBind(t, s, ytbBinding(target))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(source), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	if _, err := s.AddTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil {
		t.Fatal(err)
	}
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); pass.Handled == 0 {
		t.Fatalf("add feed pass %+v", pass)
	}
	if changed, err := s.RemoveTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); pass.Handled == 0 {
		t.Fatalf("remove feed pass %+v", pass)
	}
	p, err := ytwrite.New(ytwrite.Config{Ledger: s.YTTenantLedger(ytcTenant), Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(source)); err != nil || len(got.Related) != 0 {
		t.Fatalf("queued relation removal left stale link: %+v err=%v", got, err)
	}
}

func TestYTLinksPreservesImportedLinkWithoutAppliedOwnership(t *testing.T) {
	s := schemaStore(t, "yt_links_imported")
	ytbMapped(t, s)
	source, target := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(source))
	ytbBind(t, s, ytbBinding(target))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(source), ytwrite.IssueLinks{Related: []string{ytfIssue(target)}, Duplicates: []string{}})
	if _, err := s.AddTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RemoveTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	linksPass(t, s, remote)
	var reason string
	if err := s.pool.QueryRow(t.Context(), `SELECT reason FROM yt_write_ops WHERE principal=$1 AND entity_id=$2 ORDER BY id DESC LIMIT 1`, YTLinksPrincipal, fmt.Sprint(source)).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != ytwrite.ReasonUnownedLink {
		t.Fatalf("op reason %q, want %q", reason, ytwrite.ReasonUnownedLink)
	}
	if got, err := remote.ReadIssueLinks(t.Context(), ytfIssue(source)); err != nil || len(got.Related) != 1 || got.Related[0] != ytfIssue(target) {
		t.Fatalf("unowned imported link changed: %+v err=%v", got, err)
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
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(lower), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	remote.SetIssueLinks(ytfIssue(higher), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	if _, err := s.AddTaskRelation(t.Context(), higher, RelationRelated, lower, "test"); err != nil {
		t.Fatal(err)
	}
	linksPass(t, s, remote)
	for issue, target := range map[string]string{ytfIssue(lower): ytfIssue(higher), ytfIssue(higher): ytfIssue(lower)} {
		if got, err := remote.ReadIssueLinks(t.Context(), issue); err != nil || len(got.Related) != 1 || got.Related[0] != target {
			t.Fatalf("relation was not projected on %s: %+v err=%v", issue, got, err)
		}
	}
	if changed, err := s.RemoveTaskRelation(t.Context(), higher, RelationRelated, lower, "test"); err != nil || !changed {
		t.Fatalf("remove changed=%t err=%v", changed, err)
	}
	linksPass(t, s, remote)
	for _, issue := range []string{ytfIssue(lower), ytfIssue(higher)} {
		if got, err := remote.ReadIssueLinks(t.Context(), issue); err != nil || len(got.Related) != 0 {
			t.Fatalf("archived relation remains on %s: %+v err=%v", issue, got, err)
		}
	}
}

func TestYTLinksBindingTargetPreparesDependentTasks(t *testing.T) {
	s := schemaStore(t, "yt_links_bind_target")
	ytbMapped(t, s)
	source, target := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(source))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	if _, err := s.AddTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil {
		t.Fatal(err)
	}
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); pass.Read == 0 {
		t.Fatalf("source feed was not consumed while target was unmapped: %+v", pass)
	}
	remote := ytwritetest.NewFake()
	remote.SetIssueLinks(ytfIssue(source), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	remote.SetIssueLinks(ytfIssue(target), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	ytbBind(t, s, ytbBinding(target))
	linksPass(t, s, remote)
	for issue, targetIssue := range map[string]string{ytfIssue(source): ytfIssue(target), ytfIssue(target): ytfIssue(source)} {
		if got, err := remote.ReadIssueLinks(t.Context(), issue); err != nil || len(got.Related) != 1 || got.Related[0] != targetIssue {
			t.Fatalf("dependent relation was not projected on %s: %+v err=%v", issue, got, err)
		}
	}
}

func TestYTLinksEnableBackfillCatchesChangesConsumedWhileOff(t *testing.T) {
	s := schemaStore(t, "yt_links_backfill")
	ytbMapped(t, s)
	source, target := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(source))
	ytbBind(t, s, ytbBinding(target))
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); !pass.Created {
		t.Fatalf("initial pass %+v", pass)
	}
	if _, err := s.AddTaskRelation(t.Context(), source, RelationRelated, target, "test"); err != nil {
		t.Fatal(err)
	}
	if pass := ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer]); pass.Read == 0 {
		t.Fatalf("off-mode change was not consumed: %+v", pass)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	remote := ytwritetest.NewFake()
	for _, task := range []int64{source, target} {
		remote.SetIssueLinks(ytfIssue(task), ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}})
	}
	_, done, err := s.BackfillYTLinksOps(t.Context(), 0, 100)
	if err != nil || !done {
		t.Fatalf("backfill done=%t err=%v", done, err)
	}
	p, err := ytwrite.New(ytwrite.Config{Ledger: s.YTTenantLedger(ytcTenant), Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	for issue, targetIssue := range map[string]string{ytfIssue(source): ytfIssue(target), ytfIssue(target): ytfIssue(source)} {
		if got, err := remote.ReadIssueLinks(t.Context(), issue); err != nil || len(got.Related) != 1 || got.Related[0] != targetIssue {
			t.Fatalf("backfill missed current state on %s: %+v err=%v", issue, got, err)
		}
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

package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// These tests hold the queue-fairness rules the sync shares across tasks,
// decisions and documents: a key that fails or is skipped waits its turn
// instead of holding the head of a batch, every page of a read is seen, and a
// document follows its project.

// A failing task must not hold the head of a one-row batch: its retry waits out
// the backoff, and the next task in line is pushed on the next pass.
func TestFailingTaskDoesNotStarveLaterTasks(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	failing := fs.addTask("handoffkeep", "handoffkeep", "fails", store.StateBacklog, 4)
	passing := fs.addTask("handoffkeep", "handoffkeep", "passes", store.StateBacklog, 4)
	yt.failSummary = "fails"
	svc := newTestService(t, yt, fs)
	ctx := context.Background()
	opts := SyncOptions{TaskLimit: 1}

	first, err := svc.Sync(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("the failing task was not reported")
	}
	if _, ok := fs.mirrors[failing.ID]; ok {
		t.Fatal("the failing task should not have a mirror yet")
	}

	second, err := svc.Sync(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[passing.ID]; !ok {
		t.Fatalf("the passing task was starved behind the failing one: pushed=%+v", second.Pushed)
	}
	for _, p := range second.Pushed {
		if p.TaskID == failing.ID {
			t.Fatal("the failing task was retried before its backoff elapsed")
		}
	}
}

// A task skipped for an unmapped project parks. It does not take the batch slot
// on later passes, and it is judged again once a mapping is written.
func TestSkippedTaskParksUntilMappingChanges(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	// With no "other" fallback, an unmapped project is skipped rather than
	// routed to "other".
	delete(fs.projects, "other")
	unmapped := fs.addTask("ghost", "handoffkeep", "no mapping yet", store.StateBacklog, 4)
	mapped := fs.addTask("handoffkeep", "handoffkeep", "mapped", store.StateBacklog, 4)
	svc := newUnmappedTestService(t, yt, fs)
	ctx := context.Background()
	opts := SyncOptions{TaskLimit: 1}

	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[unmapped.ID]; ok {
		t.Fatal("an unmapped task must not be pushed")
	}
	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[mapped.ID]; !ok {
		t.Fatal("a parked task held the batch slot and starved the mapped task")
	}

	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "ghost", ShortName: "OTHER"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[unmapped.ID]; !ok {
		t.Fatal("the skip was not re-evaluated after the mapping was written")
	}
}

// An in-flight task is pushed on every pass, so a task in researching is pushed
// as developing and reviewing are.
func TestResearchingTaskIsPushedEveryPass(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "researching", store.StateResearching, 4)
	mirrorIssue(fs, yt, task.ID, "9-70", "HK-70")
	mirror := fs.mirrors[task.ID]
	synced := task.UpdatedAt
	mirror.SyncedAt = &synced
	fs.mirrors[task.ID] = mirror
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	for pass := 1; pass <= 2; pass++ {
		report, err := svc.Sync(ctx, SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pushed := false
		for _, p := range report.Pushed {
			if p.TaskID == task.ID && p.Action == "update" {
				pushed = true
			}
		}
		if !pushed {
			t.Fatalf("pass %d: a researching task was not pushed: %+v", pass, report.Pushed)
		}
	}
}

// The stalled set is read page by page, so a set past one page is seen whole
// and the sync does not treat the first page as complete.
func TestStalledTasksPastOnePageAreAllSeen(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	const total = 1005
	for i := 0; i < total; i++ {
		task := fs.addTask("handoffkeep", "handoffkeep", fmt.Sprintf("stalled %d", i), store.StateDeveloping, 4)
		fs.stalled[task.ID] = true
	}
	svc := newTestService(t, yt, fs)
	stalled, err := svc.stalledTaskIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != total {
		t.Fatalf("stalled tasks = %d, want %d (the first page was treated as complete)", len(stalled), total)
	}
}

// Every decision in a status is mirrored, not only the first page. Decisions
// share one created_at, so the keyset must also order by id to page through them.
func TestDecisionsBeyondTheFirstPageAreMirrored(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	const total = decisionPageSize + 5
	for i := 1; i <= total; i++ {
		fs.decisions = append(fs.decisions, store.DecisionRecord{ID: int64(i), Status: store.DecisionStatusOpen, Question: fmt.Sprintf("q%d", i), Lane: "handoffkeep", CreatedAt: docTestTime, UpdatedAt: docTestTime})
	}
	svc := newTestService(t, yt, fs)
	ctx := context.Background()
	var report SyncReport
	svc.syncDecisions(ctx, SyncOptions{DryRun: true}, &report)
	if len(report.Errors) != 0 {
		t.Fatalf("decision errors: %v", report.Errors)
	}
	if len(report.Decisions) != total {
		t.Fatalf("decisions mirrored = %d, want %d (only the first page was read)", len(report.Decisions), total)
	}
	seen := map[int64]bool{}
	for _, d := range report.Decisions {
		if seen[d.DecisionID] {
			t.Fatalf("decision %d read twice across pages", d.DecisionID)
		}
		seen[d.DecisionID] = true
	}
}

// A document that fails keeps its place in the queue without holding the batch:
// the next pass mirrors the document behind it.
func TestFailingDocumentDoesNotStarveLaterDocuments(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	fs.putDoc("design/a-fails", "# fails\n\nbody")
	fs.putDoc("design/b-passes", "# passes\n\nbody")
	yt.failSummary = "fails"
	svc := newTestService(t, yt, fs)
	ctx := context.Background()
	opts := SyncOptions{DocLimit: 1}

	first, err := svc.Sync(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("the failing document was not reported")
	}
	if _, ok := fs.docArticle("design/b-passes"); ok {
		t.Fatal("the first pass should have reached only the failing document")
	}
	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.docArticle("design/b-passes"); !ok {
		t.Fatal("the passing document was starved behind the failing one")
	}
}

// A document whose task has no YouTrack mapping parks; the document behind it
// is mirrored on the next pass instead of waiting for the mapping.
func TestSkippedDocumentParksAndDoesNotStarve(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	delete(fs.projects, "other")
	task := fs.addTask("ghost", "handoffkeep", "unmapped", store.StateBacklog, 4)
	fs.putDoc("design/a-skipped", "# skipped\n\nbody")
	doc := fs.docs["design/a-skipped"]
	doc.TaskID = &task.ID
	fs.docs["design/a-skipped"] = doc
	fs.putDoc("design/b-passes", "# passes\n\nbody")
	svc := newUnmappedTestService(t, yt, fs)
	ctx := context.Background()
	opts := SyncOptions{DocLimit: 1}

	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Sync(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.docArticle("design/b-passes"); !ok {
		t.Fatal("a parked document held the batch slot and starved the next one")
	}
	if _, ok := fs.docArticle("design/a-skipped"); ok {
		t.Fatal("a document with no YouTrack project must not be mirrored")
	}
}

// A document that moves to another project gets an article there. The old
// article is left in place; the ledger now names the new project.
func TestDocumentFollowsItsTaskToAnotherProject(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "moves", store.StateBacklog, 4)
	fs.putDoc("design/moved", "# moved\n\nbody")
	doc := fs.docs["design/moved"]
	doc.TaskID = &task.ID
	fs.docs["design/moved"] = doc
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	before, ok := fs.docArticle("design/moved")
	if !ok || before.HKProject != "handoffkeep" {
		t.Fatalf("first mirror = %+v, want a handoffkeep article", before)
	}

	moved := fs.tasks[task.ID]
	project := "panewire"
	moved.Project = &project
	fs.tasks[task.ID] = moved
	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	after, ok := fs.docArticle("design/moved")
	if !ok || after.HKProject != "panewire" || after.ArticleID == before.ArticleID {
		t.Fatalf("after the move the document did not get a panewire article: before=%+v after=%+v", before, after)
	}
}

// A parent index article deleted in YouTrack is written again. A document
// created after the deletion is filed under the new index, not the deleted one.
func TestDeletedParentIndexIsWrittenAgain(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	fs.putDoc("design/old", "# old\n\nbody")
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	index, ok := fs.docArticle("index:design:other")
	if !ok {
		t.Fatal("the design index was not written")
	}
	yt.mu.Lock()
	delete(yt.articles, index.ArticleID)
	yt.mu.Unlock()

	fs.putDoc("design/new", "# new\n\nbody")
	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	rewritten, ok := fs.docArticle("index:design:other")
	if !ok || rewritten.ArticleID == index.ArticleID {
		t.Fatalf("the deleted index was not replaced: before=%+v after=%+v", index, rewritten)
	}
	newDoc, ok := fs.docArticle("design/new")
	if !ok {
		t.Fatal("the new document was not mirrored")
	}
	_, _, byID, _, parents := yt.articleSnapshot()
	if _, exists := byID[rewritten.ArticleID]; !exists {
		t.Fatalf("the replacement index %s was not created in YouTrack", rewritten.ArticleID)
	}
	if got := parents[newDoc.ArticleID]; got != rewritten.ArticleID {
		t.Fatalf("new document filed under %q, want the replacement index %q", got, rewritten.ArticleID)
	}
	if strings.TrimSpace(newDoc.ArticleID) == "" {
		t.Fatal("empty article id")
	}
}

// newUnmappedTestService is newTestService with a mapped default hk project.
// The skip tests remove the "other" fallback, and the sync resolves the default
// project for its enum lookups, so the default needs a mapping of its own.
func newUnmappedTestService(t *testing.T, f *fakeYouTrack, fs *fakeStore) *Service {
	t.Helper()
	srv := f.server(t)
	client, err := New(Config{BaseURL: srv.URL, Token: "fixture-token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(fs, client, Options{Tenant: "tenant", DefaultHKProject: "handoffkeep", Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }})
}

// A skipped task whose project changes to a mapped one is pushed once its skip
// window passes, with no mapping write. A permanent park would strand it.
func TestSkippedTaskMovedToAMappedProjectIsPushed(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	delete(fs.projects, "other")
	task := fs.addTask("ghost", "handoffkeep", "moves later", store.StateBacklog, 4)
	svc := newUnmappedTestService(t, yt, fs)
	ctx := context.Background()

	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[task.ID]; ok {
		t.Fatal("an unmapped task must not be pushed")
	}

	moved := fs.tasks[task.ID]
	project := "handoffkeep"
	moved.Project = &project
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Minute)
	fs.tasks[task.ID] = moved
	fs.elapseBackoff() // the hour after the skip passes

	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.mirrors[task.ID]; !ok {
		t.Fatal("a task moved to a mapped project was stranded by its earlier skip")
	}
}

// A decision adopted from the Python state is mapped only when its issue is in
// the DEC project. The next sync writes the DEC body into whatever issue the
// mapping names, so a mapping to an issue elsewhere must be refused.
func TestAdoptDecisionRefusesAnIssueOutsideTheDecisionProject(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	yt.issues["7-1"] = Issue{ID: "7-1", IDReadable: "HK-7", Project: &ProjectRef{ID: "0-1", ShortName: "HK"}}
	yt.issues["8-1"] = Issue{ID: "8-1", IDReadable: "DEC-8", Project: &ProjectRef{ID: "0-3", ShortName: "DEC"}}
	svc := newTestService(t, yt, fs)
	ctx := context.Background()
	state := AdoptState{Decisions: map[string]AdoptDecisionRow{
		"5": {Issue: "7-1"},
		"6": {Issue: "8-1"},
	}}

	report, err := svc.Adopt(ctx, state, false)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, a := range report.Actions {
		if a.Kind == "decision" {
			actions[a.ID] = a.Action
		}
	}
	if actions["5"] != "project_mismatch" {
		t.Fatalf("decision 5 in HK project: action=%q, want project_mismatch", actions["5"])
	}
	if actions["6"] != "mapped" {
		t.Fatalf("decision 6 in DEC project: action=%q, want mapped", actions["6"])
	}
	if _, ok := fs.decisionMirrors[5]; ok {
		t.Fatal("a decision mapped to an issue outside DEC was stored")
	}
	if _, ok := fs.decisionMirrors[6]; !ok {
		t.Fatal("a decision in DEC was not stored")
	}
}

// A task adopted onto an issue that carries another task's hk ID is left
// unmapped. The next sync would write this task's content into that issue.
func TestAdoptTaskRefusesAnIssueWithAnotherTasksHKID(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "crossed", store.StateBacklog, 1)
	other := fs.addTask("handoffkeep", "handoffkeep", "other", store.StateBacklog, 1)
	yt.issues["1-1"] = Issue{ID: "1-1", IDReadable: "HK-1", Project: &ProjectRef{ID: "0-1", ShortName: "HK"}, CustomFields: []IssueField{
		{Name: "hk ID", Value: json.RawMessage(itoa(other.ID))},
	}}
	svc := newTestService(t, yt, fs)
	state := AdoptState{Issues: map[string]string{itoa(task.ID): "1-1"}}

	report, err := svc.Adopt(context.Background(), state, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || report.Actions[0].Action != "hk_id_mismatch" {
		t.Fatalf("actions=%+v, want one hk_id_mismatch", report.Actions)
	}
	if _, ok := fs.mirrors[task.ID]; ok {
		t.Fatal("a crossed task-to-issue entry was stored as a mirror")
	}
}

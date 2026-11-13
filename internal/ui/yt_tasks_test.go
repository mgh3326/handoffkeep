package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// fakeTaskIssues is a YouTrack task issue list. It serves the issues in pages
// and counts the reads.
type fakeTaskIssues struct {
	mu      sync.Mutex
	issues  []youtrack.Issue
	fail    bool
	reads   int
	queries []string
	changes []youtrack.StateChange
	// historyReads counts the StateChanges calls; sinces holds their bounds.
	historyReads int
	sinces       []time.Time
}

func (f *fakeTaskIssues) StateChanges(_ context.Context, _ youtrack.Schema, since time.Time) ([]youtrack.StateChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyReads++
	f.sinces = append(f.sinces, since)
	if f.fail {
		return nil, errors.New("youtrack GET /api/activities: 503")
	}
	return f.changes, nil
}

func (f *fakeTaskIssues) ListTaskIssues(_ context.Context, query string, skip, top int) ([]youtrack.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.queries = append(f.queries, query)
	if f.fail {
		return nil, errors.New("youtrack GET /api/issues: 503")
	}
	if skip >= len(f.issues) {
		return nil, nil
	}
	return f.issues[skip:min(skip+top, len(f.issues))], nil
}

type fakeYTStore struct {
	projects []store.YouTrackProject
	links    map[string]store.YTIssueTask
	fail     bool
}

func (f fakeYTStore) ListYouTrackProjects(context.Context) ([]store.YouTrackProject, error) {
	if f.fail {
		return nil, errors.New("db down")
	}
	return f.projects, nil
}

func (f fakeYTStore) YTIssueTaskIDs(context.Context, string) (map[string]store.YTIssueTask, error) {
	return f.links, nil
}

// ytIssue builds one task issue with the console's fields.
func ytIssue(id string, hkID int64, title, state, lane string, priority int, extra ...youtrack.IssueField) youtrack.Issue {
	fields := []youtrack.IssueField{
		{Name: "hk 상태", Value: json.RawMessage(fmt.Sprintf(`{"name":%q}`, youtrack.StateLabel(state)))},
		{Name: "레인", Value: json.RawMessage(fmt.Sprintf(`{"name":%q}`, lane))},
		{Name: "종류", Value: json.RawMessage(`{"name":"implement"}`)},
		{Name: "hk 우선순위", Value: json.RawMessage(fmt.Sprint(priority))},
		{Name: "hk 프로젝트", Value: json.RawMessage(`{"text":"handoffkeep"}`)},
	}
	if hkID > 0 {
		fields = append(fields, youtrack.IssueField{Name: "hk ID", Value: json.RawMessage(fmt.Sprint(hkID))})
	}
	fields = append(fields, extra...)
	summary := title
	if hkID > 0 {
		summary = fmt.Sprintf("#%d %s", hkID, title)
	}
	return youtrack.Issue{ID: id, IDReadable: "HK-" + id, Summary: summary,
		Created: time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC).UnixMilli(), Updated: time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC).UnixMilli(),
		CustomFields: fields}
}

func TestYTSnapshotPagesEveryIssueAndCachesForTheTTL(t *testing.T) {
	fake := &fakeTaskIssues{}
	for i := 0; i < youtrack.TaskIssuePageSize+3; i++ {
		fake.issues = append(fake.issues, ytIssue(fmt.Sprintf("2-%d", i), int64(i+1), "t", "backlog", "handoffkeep", 0))
	}
	src := newYTTaskSource(fake, fakeYTStore{projects: []store.YouTrackProject{{HKProject: "handoffkeep", ShortName: "HKW"}}}, "tenant", time.Minute)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return clock }
	snap, err := src.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != youtrack.TaskIssuePageSize+3 || fake.reads != 2 {
		t.Fatalf("tasks=%d reads=%d", len(snap.Tasks), fake.reads)
	}
	if fake.queries[0] != "(project: {HK} or project: {HKW}) sort by: created asc" {
		t.Fatalf("query=%q", fake.queries[0])
	}
	clock = clock.Add(59 * time.Second)
	if _, err := src.snapshot(context.Background()); err != nil || fake.reads != 2 {
		t.Fatalf("within TTL: err=%v reads=%d", err, fake.reads)
	}
	clock = clock.Add(2 * time.Second)
	if _, err := src.snapshot(context.Background()); err != nil || fake.reads != 4 {
		t.Fatalf("after TTL: err=%v reads=%d", err, fake.reads)
	}
}

func TestYTSnapshotFailureIsNotCachedNorServedStale(t *testing.T) {
	fake := &fakeTaskIssues{issues: []youtrack.Issue{ytIssue("2-1", 1, "t", "backlog", "handoffkeep", 0)}}
	src := newYTTaskSource(fake, fakeYTStore{}, "tenant", time.Minute)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return clock }
	if _, err := src.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	fake.fail = true
	if _, err := src.snapshot(context.Background()); !errors.Is(err, errYouTrackRead) {
		t.Fatalf("expired read with YouTrack down: err=%v, want errYouTrackRead", err)
	}
	fake.fail = false
	if _, err := src.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.reads != 3 {
		t.Fatalf("reads=%d, want 3 (the failure is not cached)", fake.reads)
	}
	db := newYTTaskSource(fake, fakeYTStore{fail: true}, "tenant", time.Minute)
	if _, err := db.snapshot(context.Background()); err == nil || errors.Is(err, errYouTrackRead) {
		t.Fatalf("store failure must not read as a YouTrack failure: %v", err)
	}
}

func TestBuildYTSnapshotPrefersHKLinksOverTheIssueField(t *testing.T) {
	issues := []youtrack.Issue{
		ytIssue("2-1", 10, "field only", "backlog", "a", 0),
		ytIssue("2-2", 99, "bound elsewhere", "backlog", "a", 0), // the binding says task 20
		ytIssue("2-3", 20, "duplicate by field", "backlog", "a", 0),
		ytIssue("2-4", 0, "no task", "backlog", "a", 0),
	}
	links := map[string]store.YTIssueTask{"2-2": {TaskID: 20, Rank: store.YTLinkBinding}}
	snap := buildYTSnapshot(youtrack.DefaultSchema(), issues, links, time.Now().UTC())
	if snap.Unbound != 1 || len(snap.Tasks) != 2 {
		t.Fatalf("snapshot=%+v", snap)
	}
	if got, ok := snap.task(20); !ok || got.IssueID != "2-2" {
		t.Fatalf("task 20=%+v, want the bound issue 2-2", got)
	}
	if got, ok := snap.task(10); !ok || got.IssueID != "2-1" {
		t.Fatalf("task 10=%+v", got)
	}
	if _, ok := snap.task(99); ok {
		t.Fatal("an issue's field must not override hk's binding")
	}
}

func TestYouTrackAttentionTasksTakesYouTrackFieldsAndDropsClosed(t *testing.T) {
	since := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	snap := buildYTSnapshot(youtrack.DefaultSchema(), []youtrack.Issue{
		ytIssue("2-1", 1, "yt title", "developing", "yt-lane", 5),
		ytIssue("2-2", 2, "closed", "completed", "a", 0),
		ytIssue("2-3", 3, "back in backlog", "backlog", "a", 0),
		ytIssue("2-4", 4, "awaiting", "backlog", "a", 1),
	}, nil, since)
	candidates := []store.AttentionTask{
		{ID: 1, Title: "hk title", Lane: "hk-lane", State: "developing", Reason: store.AttentionReasonStalled, Since: since},
		{ID: 2, Title: "closed", State: "developing", Reason: store.AttentionReasonStalled, Since: since},
		{ID: 3, Title: "stalled in hk", State: "developing", Reason: store.AttentionReasonStalled, Since: since},
		{ID: 4, Title: "awaiting", State: "backlog", Reason: store.AttentionReasonAwaitingDecision, Since: since},
		{ID: 5, Title: "no issue", State: "backlog", Reason: store.AttentionReasonAwaitingDecision, Since: since},
	}
	got := youTrackAttentionTasks(candidates, snap)
	if got.Count != 2 || got.Stalled != 1 || got.AwaitingDecision != 1 || len(got.Items) != 2 {
		t.Fatalf("tasks=%+v", got)
	}
	if x := got.Items[0]; x.ID != 1 || x.Title != "yt title" || x.Lane != "yt-lane" || x.Priority != 5 {
		t.Fatalf("item=%+v", x)
	}
}

func TestYouTrackWorkStatusJoinsHKOverlay(t *testing.T) {
	now := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	merged := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	parent := int64(3)
	snap := buildYTSnapshot(youtrack.DefaultSchema(), []youtrack.Issue{
		ytIssue("2-1", 1, "in work", "developing", "a", 0),
		ytIssue("2-2", 2, "waiting deploy", "merged", "a", 0),
		ytIssue("2-3", 3, "[bundle x B01]", "backlog", "a", 0),
		ytIssue("2-4", 4, "child", "backlog", "a", 2),
		ytIssue("2-5", 5, "done", "completed", "a", 0),
		ytIssue("2-6", 6, "queued but closed in YouTrack", "dropped", "a", 0),
	}, nil, now)
	hist := newYTHistory([]youtrack.StateChange{
		ytChange("2-1", "developing", now.Add(-time.Hour)),
		ytChange("2-2", "merged", merged),
		ytChange("2-5", "completed", time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)),
	}, now.Add(-stateHistoryLookback), now)
	overlay := store.WorkStatusOverlay{
		Inputs: store.WorkStatusInputs{
			LastDeploy: map[string]time.Time{"handoffkeep": merged.Add(-time.Hour)},
			Projects:   []string{"handoffkeep"},
			Queued:     []store.WorkStatusQueuedTask{{ID: 4, Title: "hk title", State: "backlog"}, {ID: 6, Title: "x", State: "backlog"}},
		},
		Tasks: map[int64]store.WorkStatusTaskOverlay{
			1: {JobID: "job-1", Host: "m1b", JobState: "spawned", PR: "https://github.com/o/r/pull/7"},
			4: {ParentID: &parent, ParentTitle: "hk parent title"},
		},
	}
	got := youTrackWorkStatus(now, snap, hist, overlay)
	if got.Summary.InWork != 1 || got.Summary.WaitingDeploy != 1 || got.Summary.Backlog != 1 || got.Summary.BundleParents != 1 || got.Summary.TotalOpen != 3 {
		t.Fatalf("summary=%+v", got.Summary)
	}
	if len(got.InWork) != 1 || got.InWork[0].JobID != "job-1" || got.InWork[0].Host != "m1b" || got.InWork[0].AgeSeconds != 3600 || got.InWork[0].PR == "" {
		t.Fatalf("in work=%+v", got.InWork)
	}
	if len(got.Backlog) != 1 || got.Backlog[0].BundleTitle != "[bundle x B01]" || got.Backlog[0].Priority != 2 {
		t.Fatalf("backlog=%+v", got.Backlog)
	}
	if len(got.Queued) != 1 || got.Queued[0].ID != 4 || got.Queued[0].Title != "child" {
		t.Fatalf("queued=%+v", got.Queued)
	}
	var created, closed int
	for _, day := range got.Movement {
		created += day.Created
		closed += day.Closed
	}
	// Five non-bundle issues were created in the window; one moved into a
	// closed state (the dropped one has no state change in the history).
	if len(got.Movement) != 8 || created != 5 || closed != 1 {
		t.Fatalf("movement=%+v", got.Movement)
	}
}

// ytChange is one YouTrack state change into state.
func ytChange(issueID, state string, at time.Time) youtrack.StateChange {
	return youtrack.StateChange{IssueID: issueID, At: at, State: state, StateKnown: true}
}

// An open task YouTrack shows developing again after review has been in that
// state since YouTrack's last change into it, not since an earlier visit hk
// recorded: the age follows YouTrack's log. With no change in the log the
// bound is the creation time inside the lookback and the update time past it.
func TestYouTrackStateSinceFollowsTheCurrentVisit(t *testing.T) {
	now := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	old := ytIssue("2-2", 2, "created before the lookback", "developing", "a", 0)
	old.Created = now.Add(-60 * 24 * time.Hour).UnixMilli()
	old.Updated = now.Add(-time.Hour).UnixMilli()
	snap := buildYTSnapshot(youtrack.DefaultSchema(), []youtrack.Issue{
		ytIssue("2-1", 1, "back in development", "developing", "a", 0),
		old,
		ytIssue("2-3", 3, "never changed", "developing", "a", 0),
	}, nil, now)
	hist := newYTHistory([]youtrack.StateChange{
		ytChange("2-1", "developing", now.Add(-10*time.Hour)),
		ytChange("2-1", "reviewing", now.Add(-5*time.Hour)),
		ytChange("2-1", "developing", now.Add(-2*time.Hour)),
	}, now.Add(-stateHistoryLookback), now)
	age := map[int64]int64{}
	for _, x := range youTrackWorkStatus(now, snap, hist, store.WorkStatusOverlay{}).InWork {
		age[x.ID] = x.AgeSeconds
	}
	want := map[int64]int64{
		1: 2 * 3600,
		2: int64(stateHistoryLookback.Seconds()),                                   // the history window is the earliest known bound
		3: int64(now.Sub(time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)).Seconds()), // created
	}
	for id, w := range want {
		if age[id] != w {
			t.Fatalf("task %d age=%d want %d; ages=%v", id, age[id], w, age)
		}
	}
}

// A task merged again in YouTrack waits for the deploy; a deployed task
// counts as closed on the day YouTrack recorded its deployment, however much
// later hk caught up, and on no other day.
func TestYouTrackWorkStatusUsesYouTrackTimes(t *testing.T) {
	now := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	deploy := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)    // the project's last deploy
	firstMerge := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // an earlier lifecycle
	remerged := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)  // merged again in YouTrack
	monday := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)     // 2026-11-09 KST
	tuesday := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)   // 2026-11-10 KST
	snap := buildYTSnapshot(youtrack.DefaultSchema(), []youtrack.Issue{
		ytIssue("2-1", 1, "merged again", "merged", "a", 0),
		ytIssue("2-2", 2, "completed Monday, again Tuesday", "completed", "a", 0),
		ytIssue("2-3", 3, "completed Monday, reopened", "developing", "a", 0),
		ytIssue("2-4", 4, "merged Monday, deployed Tuesday", "deployed", "a", 0),
		oldMergedIssue(now),
	}, nil, now)
	hist := newYTHistory([]youtrack.StateChange{
		ytChange("2-1", "merged", firstMerge),
		ytChange("2-1", "developing", firstMerge.Add(time.Hour)),
		ytChange("2-1", "merged", remerged),
		ytChange("2-2", "completed", monday),
		ytChange("2-2", "developing", monday.Add(time.Hour)),
		ytChange("2-2", "completed", tuesday),
		ytChange("2-3", "completed", monday),
		ytChange("2-3", "developing", tuesday),
		ytChange("2-4", "merged", monday),
		ytChange("2-4", "deployed", tuesday),
	}, now.Add(-stateHistoryLookback), now)
	oldDeploy := now.Add(-45 * 24 * time.Hour)
	overlay := store.WorkStatusOverlay{Inputs: store.WorkStatusInputs{LastDeploy: map[string]time.Time{
		"handoffkeep":  deploy,
		"stale-deploy": oldDeploy,
	}}}
	got := youTrackWorkStatus(now, snap, hist, overlay)
	if got.Summary.WaitingDeploy != 1 || got.Summary.MergedBeforeDeploy != 1 {
		t.Fatalf("a merge after the deploy must wait for deploy: summary=%+v", got.Summary)
	}
	closedOn := map[string]int{}
	for _, day := range got.Movement {
		closedOn[day.Date] = day.Closed
	}
	// Monday: tasks 2 and 3. Tuesday: tasks 2 (again) and 4 (deployed; its
	// merge on Monday is not a close).
	if closedOn["2026-11-09"] != 2 || closedOn["2026-11-10"] != 2 || closedOn["2026-11-12"] != 0 {
		t.Fatalf("movement=%+v", got.Movement)
	}
}

func oldMergedIssue(now time.Time) youtrack.Issue {
	issue := ytIssue("2-5", 5, "old merge with unknown time", "merged", "a", 0)
	issue.Created = now.Add(-90 * 24 * time.Hour).UnixMilli()
	issue.Updated = now.Add(-60 * 24 * time.Hour).UnixMilli()
	for i := range issue.CustomFields {
		if issue.CustomFields[i].Name == "hk 프로젝트" {
			issue.CustomFields[i].Value = json.RawMessage(`{"text":"stale-deploy"}`)
		}
	}
	return issue
}

// The work status reads the history once per TTL and fails with the page when
// YouTrack's activity log cannot be read.
func TestYTHistoryCachesForTheTTLAndFailsWithYouTrack(t *testing.T) {
	fake := &fakeTaskIssues{changes: []youtrack.StateChange{ytChange("2-1", "developing", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))}}
	src := newYTTaskSource(fake, fakeYTStore{}, "tenant", time.Minute)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return clock }
	h, err := src.history(context.Background())
	if err != nil || len(h.byIssue["2-1"]) != 1 {
		t.Fatalf("history=%+v err=%v", h, err)
	}
	if want := clock.Add(-stateHistoryLookback); !fake.sinces[0].Equal(want) {
		t.Fatalf("since=%s want %s", fake.sinces[0], want)
	}
	clock = clock.Add(59 * time.Second)
	if _, err := src.history(context.Background()); err != nil || fake.historyReads != 1 {
		t.Fatalf("within TTL: err=%v reads=%d", err, fake.historyReads)
	}
	clock = clock.Add(2 * time.Second)
	fake.fail = true
	if _, err := src.history(context.Background()); !errors.Is(err, errYouTrackRead) {
		t.Fatalf("after TTL with the log down: err=%v", err)
	}
	if _, err := src.history(context.Background()); !errors.Is(err, errYouTrackRead) || fake.historyReads != 3 {
		t.Fatalf("a failed read is not cached: err=%v reads=%d", err, fake.historyReads)
	}
}

func TestYouTrackReadFailedAnswers502ForYouTrackOnly(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: down", errYouTrackRead), http.StatusBadGateway, "youtrack_unavailable"},
		{errors.New("db down"), http.StatusInternalServerError, "internal_error"},
	} {
		rec := httptest.NewRecorder()
		youTrackReadFailed(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks", nil), tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Fatalf("err=%v: %d %s", tc.err, rec.Code, rec.Body.String())
		}
	}
}

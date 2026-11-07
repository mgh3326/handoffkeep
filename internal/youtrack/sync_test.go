package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestSyncPushCreatesIssueWithKoreanSummaryAndComments(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "English title", store.StateBacklog, 3)
	fs.translations[task.ID] = store.Translation{Entity: "task", EntityID: strconv.FormatInt(task.ID, 10), Field: "title", Lang: "ko", Text: "한국어 제목", Translator: "test"}
	fs.comments[task.ID] = []store.TaskComment{{ID: 1, TaskID: task.ID, Body: "첫 코멘트", Author: "worker", CreatedAt: time.Now().UTC()}}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if yt.createCalls != 1 {
		t.Fatalf("createCalls=%d want 1", yt.createCalls)
	}
	created := yt.issues["1-1"]
	if created.Summary != "한국어 제목" {
		t.Fatalf("summary=%q", created.Summary)
	}
	if !strings.HasPrefix(created.Description, "https://console.robinco.dev/ui/tasks/"+strconv.FormatInt(task.ID, 10)+"\n\n") {
		t.Fatalf("description=%q", created.Description)
	}
	if !strings.Contains(created.Description, "English title") {
		t.Fatalf("description lost the original title: %q", created.Description)
	}
	if len(yt.comments) != 1 || yt.comments[0] != "첫 코멘트" {
		t.Fatalf("comments=%v", yt.comments)
	}
	link, ok := fs.links[task.ID]
	if !ok || link.ReadableID != "HK-1" {
		t.Fatalf("link=%+v ok=%v", link, ok)
	}
	if !fs.pushed[task.ID] {
		t.Fatal("task was not marked pushed")
	}
}

func TestSyncSkipsBotActivity(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "bot target", store.StateBacklog, 0)
	fs.link(task.ID, "9-1", "HK-1")
	activity := stateActivity("a-bot", "HK-1", "대기", "종료됨")
	activity.Author = &User{ID: "bot-id", Login: "hk-bot"}
	yt.activities = []Activity{activity}
	yt.cursor = "c1"
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.applied) != 0 {
		t.Fatalf("bot activity was applied: %+v", fs.applied)
	}
	if len(report.Pulled) != 0 {
		t.Fatalf("pulled=%+v", report.Pulled)
	}
}

func TestSyncAppliesStatePriorityLaneAndComment(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "apply target", store.StateBacklog, 0)
	fs.link(task.ID, "9-7", "HK-7")
	yt.activities = []Activity{{
		ID: "a-multi", Author: operator(), Target: &Issue{ID: "9-7", IDReadable: "HK-7"},
		Categories: []ActivityCategory{
			{Type: "CustomFieldCategory", Field: &ActivityField{Name: "hk 상태"}, Added: []json.RawMessage{json.RawMessage(`{"name":"종료됨"}`)}},
			{Type: "CustomFieldCategory", Field: &ActivityField{Name: "hk 우선순위"}, Added: []json.RawMessage{json.RawMessage(`5`)}},
			{Type: "CustomFieldCategory", Field: &ActivityField{Name: "레인"}, Added: []json.RawMessage{json.RawMessage(`{"name":"rstack"}`)}},
			{Type: "CommentsCategory", Added: []json.RawMessage{json.RawMessage(`{"id":"c-9","text":"안녕하세요"}`)}},
		},
	}}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	got := fs.tasks[task.ID]
	if got.State != store.StateDropped || got.Priority != 5 || got.Lane != "rstack" {
		t.Fatalf("task=%+v", got)
	}
	if len(fs.comments[task.ID]) != 1 || fs.comments[task.ID][0].Author != store.YouTrackOwnedActor {
		t.Fatalf("comments=%+v", fs.comments[task.ID])
	}
}

func TestSyncRefusalRevertsAndPostsKoreanComment(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "refusal target", store.StateBacklog, 0)
	fs.link(task.ID, "9-8", "HK-8")
	yt.activities = []Activity{stateActivity("a-refuse", "HK-8", "대기", "개발 중")}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if fs.tasks[task.ID].State != store.StateBacklog {
		t.Fatalf("state changed despite refusal: %s", fs.tasks[task.ID].State)
	}
	if len(yt.issueUpdates) != 1 {
		t.Fatalf("revert updates=%v", yt.issueUpdates)
	}
	update, _ := json.Marshal(yt.issueUpdates[0])
	if !strings.Contains(string(update), "대기") {
		t.Fatalf("revert did not write the hk state: %s", update)
	}
	if len(yt.comments) != 1 || !strings.HasPrefix(yt.comments[0], "hk가 거부: ") {
		t.Fatalf("comments=%v", yt.comments)
	}
	if report.Reverted != 1 {
		t.Fatalf("report.Reverted=%d", report.Reverted)
	}
	if len(fs.pending) != 0 {
		t.Fatalf("pending reverts were not cleared: %+v", fs.pending)
	}
}

func TestSyncSkipsDecisionAnswersAndInstructions(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "comment target", store.StateBacklog, 0)
	fs.link(task.ID, "9-9", "HK-9")
	comment := func(id, text string) Activity {
		return Activity{
			ID: id, Author: operator(), Target: &Issue{ID: "9-9", IDReadable: "HK-9"},
			Categories: []ActivityCategory{{Type: "CommentsCategory", Added: []json.RawMessage{json.RawMessage(`{"id":"` + id + `-c","text":"` + text + `"}`)}}},
		}
	}
	yt.activities = []Activity{comment("a-ins", "/지시 배포해"), comment("a-ans", "/답 A"), comment("a-normal", "일반 코멘트")}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 2 {
		t.Fatalf("report.Skipped=%d pulled=%+v", report.Skipped, report.Pulled)
	}
	if len(fs.comments[task.ID]) != 1 || fs.comments[task.ID][0].Body != "일반 코멘트" {
		t.Fatalf("comments=%+v", fs.comments[task.ID])
	}
	for _, text := range yt.comments {
		if strings.Contains(text, "지시") || strings.Contains(text, "답") {
			t.Fatalf("skipped comment was mirrored back: %q", text)
		}
	}
}

func TestSyncCreatesTaskForOperatorIssue(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	yt.activities = []Activity{{
		ID: "a-new", Author: operator(),
		Categories: []ActivityCategory{{Type: "IssueCreatedCategory"}},
		Target: &Issue{ID: "9-77", IDReadable: "HK-77", Summary: "운영자 새 이슈", CustomFields: []IssueField{
			{Name: "레인", Value: json.RawMessage(`{"name":"operator-desk"}`)},
			{Name: "종류", Value: json.RawMessage(`{"name":"fix"}`)},
			{Name: "hk 프로젝트", Value: json.RawMessage(`{"name":"handoffkeep"}`)},
		}},
	}}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	var created store.Task
	for _, task := range fs.tasks {
		if task.Title == "운영자 새 이슈" {
			created = task
		}
	}
	if created.ID == 0 || created.State != store.StateTriage || created.Lane != "operator-desk" || created.Kind != "fix" {
		t.Fatalf("created=%+v", created)
	}
	if fs.links[created.ID].ReadableID != "HK-77" {
		t.Fatalf("link=%+v", fs.links[created.ID])
	}
}

func TestSyncReplayAppliesOnce(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "replay target", store.StateBacklog, 0)
	fs.link(task.ID, "9-10", "HK-10")
	yt.activities = []Activity{{
		ID: "a-priority", Author: operator(), Target: &Issue{ID: "9-10", IDReadable: "HK-10"},
		Categories: []ActivityCategory{{Type: "CustomFieldCategory", Field: &ActivityField{Name: "hk 우선순위"}, Added: []json.RawMessage{json.RawMessage(`9`)}}},
	}}
	svc := newTestService(t, yt, fs)

	first, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Pulled) != 1 || first.Pulled[0].Outcome != store.YouTrackOutcomeApplied {
		t.Fatalf("first=%+v", first.Pulled)
	}
	second, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Pulled) != 1 || second.Pulled[0].Outcome != store.YouTrackOutcomeDuplicate {
		t.Fatalf("second=%+v", second.Pulled)
	}
	if fs.tasks[task.ID].Priority != 9 {
		t.Fatalf("priority=%d", fs.tasks[task.ID].Priority)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "dry target", store.StateBacklog, 2)
	fs.link(task.ID, "9-11", "HK-11")
	yt.activities = []Activity{stateActivity("a-dry", "HK-11", "대기", "종료됨")}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(report)
	t.Logf("dry-run report: %s", data)
	if !report.DryRun || len(report.Pulled) != 1 || report.Pulled[0].Outcome != "would_apply" {
		t.Fatalf("report=%+v", report)
	}
	if len(fs.applied) != 0 || len(yt.issueUpdates) != 0 || len(yt.comments) != 0 || yt.createCalls != 0 {
		t.Fatalf("dry-run wrote: applied=%v updates=%v comments=%v creates=%d", fs.applied, yt.issueUpdates, yt.comments, yt.createCalls)
	}
	if fs.tasks[task.ID].State != store.StateBacklog {
		t.Fatalf("dry-run changed hk state: %s", fs.tasks[task.ID].State)
	}
}

func TestOverlayUsesCacheAndLeavesUnlinkedTasksAlone(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	linked := fs.addTask("handoffkeep", "linked", store.StateBacklog, 1)
	unlinked := fs.addTask("handoffkeep", "unlinked", store.StateBacklog, 2)
	fs.link(linked.ID, "9-20", "HK-20")
	fs.caches[linked.ID] = store.YouTrackFieldCache{TaskID: linked.ID, Priority: 7, Lane: "rstack", FetchedAt: time.Now().UTC()}
	fs.caches[unlinked.ID] = store.YouTrackFieldCache{TaskID: unlinked.ID, Priority: 8, Lane: "nope", FetchedAt: time.Now().UTC()}
	svc := newTestService(t, yt, fs)

	tasks := []store.Task{fs.tasks[linked.ID], fs.tasks[unlinked.ID]}
	svc.Overlay(context.Background(), tasks)
	if tasks[0].Priority != 7 || tasks[0].Lane != "rstack" || tasks[0].PrioritySource != "youtrack" || tasks[0].LaneSource != "youtrack" || tasks[0].Stale {
		t.Fatalf("linked overlay=%+v", tasks[0])
	}
	if tasks[1].Priority != 2 || tasks[1].Lane != "handoffkeep" || tasks[1].PrioritySource != "" || tasks[1].Stale {
		t.Fatalf("unlinked changed=%+v", tasks[1])
	}
}

func TestOverlayMarksStaleOnFetchError(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "stale target", store.StateBacklog, 4)
	fs.link(task.ID, "9-30", "HK-30")
	svc := newTestService(t, yt, fs)
	yt.failUpdate = true

	tasks := []store.Task{fs.tasks[task.ID]}
	svc.Overlay(context.Background(), tasks)
	if tasks[0].Priority != 4 || !tasks[0].Stale || tasks[0].PrioritySource != "youtrack" {
		t.Fatalf("overlay=%+v", tasks[0])
	}
}

func TestPreparePriorityAndLaneWriteThrough(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	linked := fs.addTask("handoffkeep", "linked", store.StateBacklog, 1)
	unlinked := fs.addTask("handoffkeep", "unlinked", store.StateBacklog, 2)
	fs.link(linked.ID, "9-40", "HK-40")
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	if err := svc.PreparePriority(ctx, unlinked.ID, 6); err != nil {
		t.Fatal(err)
	}
	if len(yt.issueUpdates) != 0 {
		t.Fatalf("an unlinked task wrote YouTrack: %v", yt.issueUpdates)
	}
	if err := svc.PreparePriority(ctx, linked.ID, 6); err != nil {
		t.Fatal(err)
	}
	if len(yt.issueUpdates) != 1 {
		t.Fatalf("updates=%v", yt.issueUpdates)
	}
	body, _ := json.Marshal(yt.issueUpdates[0])
	if !strings.Contains(string(body), "hk 우선순위") || !strings.Contains(string(body), "6") {
		t.Fatalf("priority body=%s", body)
	}

	if err := svc.PrepareLane(ctx, linked.ID, "ops"); err != nil {
		t.Fatal(err)
	}
	if len(yt.bundleAdds) != 1 || yt.bundleAdds[0] != "ops" {
		t.Fatalf("bundle adds=%v", yt.bundleAdds)
	}
	if len(yt.issueUpdates) != 2 {
		t.Fatalf("updates=%v", yt.issueUpdates)
	}
}

func TestPreparePriorityFailureReturnsUnavailableAndWritesNothingLocal(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "write target", store.StateBacklog, 3)
	fs.link(task.ID, "9-50", "HK-50")
	svc := newTestService(t, yt, fs)
	yt.failUpdate = true

	err := svc.PreparePriority(context.Background(), task.ID, 9)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
	if fs.tasks[task.ID].Priority != 3 {
		t.Fatalf("hk priority changed on a YouTrack failure: %d", fs.tasks[task.ID].Priority)
	}
	if c, ok := fs.caches[task.ID]; ok && c.Priority != 0 && c.Priority != 3 {
		t.Fatalf("cache changed on a YouTrack failure: %+v", c)
	}
}

func TestServiceLogsNeverContainToken(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "log target", store.StateBacklog, 0)
	fs.link(task.ID, "9-60", "HK-60")
	yt.failMe = true
	svc := newTestService(t, yt, fs)
	_, err := svc.Sync(context.Background(), SyncOptions{})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), "fixture-token") || strings.Contains(buf.String(), "fixture-token") {
		t.Fatalf("token leaked: err=%q log=%q", err, buf.String())
	}
}

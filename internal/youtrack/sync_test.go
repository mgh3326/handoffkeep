package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
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
	task := fs.addTask("handoffkeep", "handoffkeep", "English title", store.StateBacklog, 3)
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
	if created.Project == nil || created.Project.ShortName != "HK" {
		t.Fatalf("project=%+v", created.Project)
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
	mirror, ok := fs.mirrors[task.ID]
	if !ok || mirror.DisplayRef != "HK-1" || mirror.Role != "mirror" {
		t.Fatalf("mirror=%+v ok=%v", mirror, ok)
	}
	if !fs.pushed[task.ID] {
		t.Fatal("task was not marked synced")
	}
}

func TestSyncSkipsBotActivity(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "bot target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-1", "HK-1")
	activity := stateActivity("a-bot", "9-1", "HK-1", "종료됨")
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
	task := fs.addTask("handoffkeep", "handoffkeep", "apply target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-7", "HK-7")
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
	task := fs.addTask("handoffkeep", "handoffkeep", "refusal target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-8", "HK-8")
	yt.activities = []Activity{stateActivity("a-refuse", "9-8", "HK-8", "개발 중")}
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
	task := fs.addTask("handoffkeep", "handoffkeep", "comment target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-9", "HK-9")
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
	issue := &Issue{ID: "9-77", IDReadable: "HK-77", Summary: "운영자 새 이슈", Project: &ProjectRef{ID: "0-1", ShortName: "HK"}, CustomFields: []IssueField{
		{Name: "레인", Value: json.RawMessage(`{"name":"operator-desk"}`)},
		{Name: "종류", Value: json.RawMessage(`{"name":"fix"}`)},
	}}
	// The pull creates the hk task; the same run's cache refresh reads the
	// issue, so the fake serves it like YouTrack would.
	yt.issues[issue.ID] = *issue
	yt.activities = []Activity{{
		ID: "a-new", Author: operator(),
		Categories: []ActivityCategory{{Type: "IssueCreatedCategory"}},
		Target:     issue,
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
	if created.ID == 0 || created.State != store.StateTriage || created.Lane != "operator-desk" || created.Kind != "fix" || created.Project == nil || *created.Project != "handoffkeep" {
		t.Fatalf("created=%+v", created)
	}
	if fs.mirrors[created.ID].DisplayRef != "HK-77" {
		t.Fatalf("mirror=%+v", fs.mirrors[created.ID])
	}
}

func TestSyncReplayAppliesOnce(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "replay target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-10", "HK-10")
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
	task := fs.addTask("handoffkeep", "handoffkeep", "dry target", store.StateBacklog, 2)
	mirrorIssue(fs, yt, task.ID, "9-11", "HK-11")
	yt.activities = []Activity{stateActivity("a-dry", "9-11", "HK-11", "종료됨")}
	// Make the task due so the dry-run push resolves its project too.
	moved := fs.tasks[task.ID]
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Minute)
	fs.tasks[task.ID] = moved
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
	if fs.writes != 0 {
		t.Fatalf("dry-run wrote %d rows", fs.writes)
	}
	if len(fs.advanced) != 0 {
		t.Fatalf("dry-run advanced the cursor: %v", fs.advanced)
	}
	if fs.projectIDs != 0 {
		t.Fatalf("dry-run persisted %d project ids", fs.projectIDs)
	}
}

// pushedFieldValue reads one custom field out of a recorded update body.
func pushedFieldValue(body map[string]any, name string) any {
	fields, _ := body["customFields"].([]any)
	for _, raw := range fields {
		field, _ := raw.(map[string]any)
		if field["name"] == name {
			return field["value"]
		}
	}
	return nil
}

func TestPushSendsPGValuesNotTheCache(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "push source", store.StateBacklog, 3)
	mirrorIssue(fs, yt, task.ID, "9-80", "HK-80")
	// The cache holds an older YouTrack read; PG is the execution value.
	fs.caches[task.ID] = store.YouTrackFieldCache{TaskID: task.ID, Priority: 0, Lane: "handoffkeep", Source: store.YouTrackSourceYouTrack, FetchedAt: time.Now().UTC()}
	moved := fs.tasks[task.ID]
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Minute)
	fs.tasks[task.ID] = moved
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if len(yt.issueUpdates) != 1 {
		t.Fatalf("updates=%v", yt.issueUpdates)
	}
	if got, _ := pushedFieldValue(yt.issueUpdates[0], "hk 우선순위").(float64); got != 3 {
		t.Fatalf("pushed priority=%v want 3 (update=%v)", got, yt.issueUpdates[0])
	}
	if cache := fs.caches[task.ID]; cache.Priority != 3 || cache.Source != store.YouTrackSourceHK {
		t.Fatalf("cache=%+v", cache)
	}
}

func TestSyncPullsBeforeItPushes(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "order target", store.StateBacklog, 2)
	mirrorIssue(fs, yt, task.ID, "9-81", "HK-81")
	moved := fs.tasks[task.ID]
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Minute)
	fs.tasks[task.ID] = moved
	yt.activities = []Activity{{
		ID: "a-order", Author: operator(), Target: &Issue{ID: "9-81", IDReadable: "HK-81"},
		Categories: []ActivityCategory{{Type: "CustomFieldCategory", Field: &ActivityField{Name: "hk 우선순위"}, Added: []json.RawMessage{json.RawMessage(`1`)}}},
	}}
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if got := fs.tasks[task.ID].Priority; got != 1 {
		t.Fatalf("PG priority=%d want the operator's 1", got)
	}
	if len(yt.issueUpdates) != 1 {
		t.Fatalf("updates=%v", yt.issueUpdates)
	}
	if got, _ := pushedFieldValue(yt.issueUpdates[0], "hk 우선순위").(float64); got != 1 {
		t.Fatalf("YouTrack was pushed %v but PG is 1 (update=%v)", got, yt.issueUpdates[0])
	}
}

func TestSyncConsumesAnActivityItCannotApply(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "skip target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-82", "HK-82")
	yt.activities = []Activity{
		{
			ID: "a-bad", Author: operator(), Target: &Issue{ID: "9-82", IDReadable: "HK-82"},
			Categories: []ActivityCategory{{Type: "CustomFieldCategory", Field: &ActivityField{Name: "레인"}, Added: []json.RawMessage{json.RawMessage(`{"name":"Ops Desk"}`)}}},
		},
		{
			ID: "a-good", Author: operator(), Target: &Issue{ID: "9-82", IDReadable: "HK-82"},
			Categories: []ActivityCategory{{Type: "CustomFieldCategory", Field: &ActivityField{Name: "hk 우선순위"}, Added: []json.RawMessage{json.RawMessage(`6`)}}},
		},
	}
	yt.cursor = "c-2"
	fs.applyErr["a-bad"] = store.ErrYouTrackActivityInput
	svc := newTestService(t, yt, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if report.Skipped != 1 {
		t.Fatalf("skipped=%d pulled=%+v", report.Skipped, report.Pulled)
	}
	if fs.tasks[task.ID].Priority != 6 {
		t.Fatalf("a later activity was blocked: %+v", fs.tasks[task.ID])
	}
	if len(fs.advanced) != 1 || fs.advanced[0] != "c-2" {
		t.Fatalf("cursor=%v", fs.advanced)
	}
	if _, ok := fs.applied["a-bad"]; !ok {
		t.Fatalf("the unappliable activity was not recorded: %+v", fs.applied)
	}
}

func TestOverlaySeedLimitMarksTheRestStale(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	first := fs.addTask("handoffkeep", "handoffkeep", "first", store.StateBacklog, 1)
	second := fs.addTask("handoffkeep", "handoffkeep", "second", store.StateBacklog, 2)
	third := fs.addTask("handoffkeep", "handoffkeep", "third", store.StateBacklog, 3)
	mirrorIssue(fs, yt, first.ID, "9-70", "HK-70")
	mirrorIssue(fs, yt, second.ID, "9-71", "HK-71")
	mirrorIssue(fs, yt, third.ID, "9-72", "HK-72")
	svc := newTestService(t, yt, fs)
	svc.seeds = 1

	tasks := []store.Task{fs.tasks[first.ID], fs.tasks[second.ID], fs.tasks[third.ID]}
	svc.Overlay(context.Background(), tasks)
	if tasks[0].PrioritySource != store.YouTrackSourceYouTrack || tasks[0].Stale {
		t.Fatalf("seeded task=%+v", tasks[0])
	}
	for _, task := range tasks[1:] {
		if task.PrioritySource != store.YouTrackSourceHK || task.LaneSource != store.YouTrackSourceHK || !task.Stale {
			t.Fatalf("unseeded task=%+v", task)
		}
	}
}

func TestOverlayUsesCacheAndLeavesUnmirroredTasksAlone(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	mirrored := fs.addTask("handoffkeep", "handoffkeep", "mirrored", store.StateBacklog, 1)
	unmirrored := fs.addTask("handoffkeep", "handoffkeep", "unmirrored", store.StateBacklog, 2)
	mirrorIssue(fs, yt, mirrored.ID, "9-20", "HK-20")
	now := time.Now().UTC()
	fs.caches[mirrored.ID] = store.YouTrackFieldCache{TaskID: mirrored.ID, Priority: 7, Lane: "rstack", Source: store.YouTrackSourceYouTrack, FetchedAt: now}
	fs.caches[unmirrored.ID] = store.YouTrackFieldCache{TaskID: unmirrored.ID, Priority: 8, Lane: "nope", Source: store.YouTrackSourceYouTrack, FetchedAt: now}
	svc := newTestService(t, yt, fs)

	tasks := []store.Task{fs.tasks[mirrored.ID], fs.tasks[unmirrored.ID]}
	svc.Overlay(context.Background(), tasks)
	if tasks[0].Priority != 7 || tasks[0].Lane != "rstack" || tasks[0].PrioritySource != store.YouTrackSourceYouTrack || tasks[0].LaneSource != store.YouTrackSourceYouTrack || tasks[0].Stale || tasks[0].ValuesFetchedAt == nil {
		t.Fatalf("mirrored overlay=%+v", tasks[0])
	}
	if tasks[1].Priority != 2 || tasks[1].Lane != "handoffkeep" || tasks[1].PrioritySource != "" || tasks[1].Stale {
		t.Fatalf("unmirrored changed=%+v", tasks[1])
	}
}

func TestOverlayMarksStaleOnFetchError(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "stale target", store.StateBacklog, 4)
	fs.mirror(task.ID, "9-30", "HK-30")
	svc := newTestService(t, yt, fs)

	tasks := []store.Task{fs.tasks[task.ID]}
	svc.Overlay(context.Background(), tasks)
	if tasks[0].Priority != 4 || !tasks[0].Stale || tasks[0].PrioritySource != store.YouTrackSourceHK {
		t.Fatalf("overlay=%+v", tasks[0])
	}
	// The stale row is persisted, so the next read does not repeat the stall.
	if cache, ok := fs.caches[task.ID]; !ok || !cache.Stale || cache.Source != store.YouTrackSourceHK {
		t.Fatalf("stale cache was not written: %+v ok=%v", cache, ok)
	}
}

func TestRecordPriorityAndLaneQueueOutbox(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	mirrored := fs.addTask("handoffkeep", "handoffkeep", "mirrored", store.StateBacklog, 1)
	unmirrored := fs.addTask("handoffkeep", "handoffkeep", "unmirrored", store.StateBacklog, 2)
	mirrorIssue(fs, yt, mirrored.ID, "9-40", "HK-40")
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	svc.RecordPriority(ctx, unmirrored.ID, 6)
	if len(fs.outbox) != 0 {
		t.Fatalf("an unmirrored task queued an outbox row: %+v", fs.outbox)
	}
	svc.RecordPriority(ctx, mirrored.ID, 6)
	svc.RecordLane(ctx, mirrored.ID, "ops")
	if len(fs.outbox) != 2 {
		t.Fatalf("outbox=%+v", fs.outbox)
	}
	if cache := fs.caches[mirrored.ID]; cache.Priority != 6 || cache.Lane != "ops" || cache.Source != store.YouTrackSourceHK {
		t.Fatalf("cache=%+v", cache)
	}

	// The next sync drains the outbox. YouTrack receives both writes.
	report, err := svc.Sync(ctx, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if len(yt.issueUpdates) != 2 {
		t.Fatalf("issue updates=%v", yt.issueUpdates)
	}
	if due, _ := fs.DueYouTrackOutbox(ctx, 10); len(due) != 0 {
		t.Fatalf("outbox not drained: %+v", due)
	}
}

func TestOutboxFailureDoesNotChangeHK(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "write target", store.StateBacklog, 3)
	mirrorIssue(fs, yt, task.ID, "9-50", "HK-50")
	svc := newTestService(t, yt, fs)
	ctx := context.Background()
	yt.failUpdate = true

	svc.RecordPriority(ctx, task.ID, 9)
	if fs.tasks[task.ID].Priority != 3 {
		t.Fatalf("RecordPriority changed the PG value: %d", fs.tasks[task.ID].Priority)
	}
	report, err := svc.Sync(ctx, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) == 0 {
		t.Fatal("a YouTrack outage was not reported")
	}
	if fs.tasks[task.ID].Priority != 3 {
		t.Fatalf("hk priority changed on a YouTrack failure: %d", fs.tasks[task.ID].Priority)
	}
	var retried bool
	for _, item := range fs.outbox {
		if item.Attempts > 0 {
			retried = true
		}
	}
	if !retried {
		t.Fatalf("outbox was not rescheduled: %+v", fs.outbox)
	}
}

func TestSyncUsesMappedProjectsAndMovesOnChange(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "project target", store.StateBacklog, 0)
	svc := newTestService(t, yt, fs)

	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	created := yt.issues["1-1"]
	if created.Project == nil || created.Project.ShortName != "HK" {
		t.Fatalf("created project=%+v", created.Project)
	}

	// Moving the hk project moves the issue; the stable id keeps the link.
	moved := fs.tasks[task.ID]
	project := "panewire"
	moved.Project = &project
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Hour)
	fs.tasks[task.ID] = moved
	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if yt.issues["1-1"].Project == nil || yt.issues["1-1"].Project.ShortName != "PW" {
		t.Fatalf("issue project=%+v", yt.issues["1-1"].Project)
	}
	if got := fs.mirrors[task.ID]; got.IssueID != "1-1" || got.DisplayRef != "PW-1" {
		t.Fatalf("mirror=%+v", got)
	}

	// An unmapped hk project goes to OTHER.
	other := fs.addTask("unmapped-project", "handoffkeep", "other target", store.StateBacklog, 0)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	var otherIssue Issue
	for _, issue := range yt.issues {
		if issue.Summary == "other target" {
			otherIssue = issue
		}
	}
	if otherIssue.ID == "" || otherIssue.Project == nil || otherIssue.Project.ShortName != "OTHER" {
		t.Fatalf("other issue=%+v (task %d)", otherIssue, other.ID)
	}
}

func TestFailedFieldsAfterAMoveStayDue(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "move then fail", store.StateBacklog, 4)
	mirrorIssue(fs, yt, task.ID, "9-90", "HK-90")
	// A fixed base makes the watermark comparison independent of the clock.
	base := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	mirror := fs.mirrors[task.ID]
	mirror.SyncedAt = &base
	fs.mirrors[task.ID] = mirror
	moved := fs.tasks[task.ID]
	project := "panewire"
	moved.Project = &project
	moved.UpdatedAt = base.Add(time.Minute)
	fs.tasks[task.ID] = moved
	yt.failFieldUpdate = true
	svc := newTestService(t, yt, fs)
	ctx := context.Background()

	report, err := svc.Sync(ctx, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) == 0 {
		t.Fatal("the field update failure was not reported")
	}
	if got := fs.mirrors[task.ID]; got.DisplayRef != "PW-1" {
		t.Fatalf("the move did not update the display ref: %+v", got)
	}
	due, _ := fs.DueYouTrackPushTasks(ctx, 10)
	var found bool
	for _, candidate := range due {
		if candidate.ID == task.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unsent field push is no longer due: %+v", fs.mirrors[task.ID])
	}

	// The next run retries and settles the task.
	yt.failFieldUpdate = false
	if _, err := svc.Sync(ctx, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if due, _ := fs.DueYouTrackPushTasks(ctx, 10); len(due) != 0 {
		t.Fatalf("the retried push did not settle: %+v", due)
	}
}

func TestServiceLogsNeverContainToken(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "log target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-60", "HK-60")
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

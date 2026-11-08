package youtrack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func ptr[T any](v T) *T { return &v }

// The tests below pin the parity with the Python mirror: the text an issue
// carries, the identity that keeps one issue per task, the one-time adoption,
// the DEC decision issues, and an idempotent re-run.

func TestCleanTitleDropsHKNumberAndGradeMarker(t *testing.T) {
	cases := map[string]string{
		"[hk 3365] YouTrack 미러 (grade B, T2)": "YouTrack 미러",
		"English title (T2)":                  "English title",
		"plain":                               "plain",
	}
	for in, want := range cases {
		if got := cleanTitle(in); got != want {
			t.Fatalf("cleanTitle(%q)=%q want %q", in, got, want)
		}
	}
	long := strings.Repeat("가", 300)
	if got := cleanTitle(long); len([]rune(got)) != summaryLimit {
		t.Fatalf("summary not cut to %d characters: %d", summaryLimit, len([]rune(got)))
	}
}

func TestGradeFromTitleNeedsTheGrade(t *testing.T) {
	if g, tier := gradeFromTitle("x (grade A+, T0)"); g != "A+" || tier != "T0" {
		t.Fatalf("grade=%q tier=%q", g, tier)
	}
	if g, tier := gradeFromTitle("x (T2)"); g != "" || tier != "" {
		t.Fatalf("a tier alone must not set the grade: %q %q", g, tier)
	}
}

func TestMachineryCommentsAreNotMirrored(t *testing.T) {
	for _, body := range []string{"spawned request=j1", "  [mergewake] merged", "panewire job.completed x", "ESC answer: yes"} {
		if humanComment(body) {
			t.Fatalf("%q should not be mirrored", body)
		}
	}
	if !humanComment("운영자: 다음 단계 진행") {
		t.Fatal("a human comment was filtered")
	}
}

func TestBriefSectionsQuoteWhatTheIssueNeeds(t *testing.T) {
	body := "Why:\n이유 본문\n\nDesired:\n원하는 결과\n\n## Acceptance criteria\n- 조건 A\n- 조건 B\n\n## Other\nx\n"
	secs := quotedBriefSections(body)
	if len(secs) != 3 {
		t.Fatalf("sections=%q", secs)
	}
	if secs[0] != "이유 본문" || secs[1] != "원하는 결과" || secs[2] != "- 조건 A\n- 조건 B" {
		t.Fatalf("sections=%q", secs)
	}
	// A brief without Why quotes the next sections in the first slots, as the
	// Python mirror's positions did.
	if got := quotedBriefSections("Desired:\n원함\n"); len(got) != 1 || got[0] != "원함" {
		t.Fatalf("sections=%q", got)
	}
}

func TestIssueTextFollowsThePythonLayout(t *testing.T) {
	job := "job-1"
	started := time.Date(2026, 11, 7, 1, 2, 0, 0, time.UTC)
	task := store.Task{ID: 9, Title: "t", State: store.StateReviewing, Refs: store.TaskRefs{JobID: job}, StartedAt: &started, LastActivityAt: started}
	events := []store.TaskEvent{
		{ID: 1, From: store.StateBacklog, To: store.StateBacklog, By: "x", At: started},
		{ID: 2, From: store.StateDeveloping, To: store.StateReviewing, By: "agent", At: started.Add(time.Hour)},
	}
	comments := []store.TaskComment{{ID: 4, Author: "worker", Body: "host=mbp profile=haiku@high", CreatedAt: started}}
	pr := &prView{Repo: "o/r", Number: 7, URL: "https://github.com/o/r/pull/7", Title: "PR title", State: "open", Head: "abc1234", Gate: "blocked", Reasons: []string{"checks"}, Fails: []string{"ci-test"}, Findings: 2, High: 1}
	text := descriptionFor(issueText{Task: task, Console: "https://console.robinco.dev", BriefKey: "brief/2026-11-07/job-1",
		Brief: "Why:\n이유\nDesired:\n결과\n## Acceptance\n- 조건\n", PR: pr, Comments: comments, Events: events})

	ordered := []string{
		"[hk 콘솔에서 열기](https://console.robinco.dev/ui/tasks/9)",
		"## 하는 일", "## 바라는 결과", "## 완료 조건", "브리프: `brief/2026-11-07/job-1`",
		"## PR", "- [o/r#7](https://github.com/o/r/pull/7) PR title", "- 상태: 열림 · head `abc1234`",
		"- 머지 판정: blocked (CI 실패)", "- 실패한 CI: ci-test", "- 열린 리뷰 지적: 2건 (높음 이상 1건)",
		"## 진행", "- 워커: `job-1` (mbp · haiku@high)", "- 시작: 2026-11-07 01:02",
		"## 상태 기록", "- 2026-11-07 02:02 개발 중 → 검토 중 (agent)",
		"<sub>원문 제목: t</sub>",
	}
	at := 0
	for _, want := range ordered {
		i := strings.Index(text[at:], want)
		if i < 0 {
			t.Fatalf("missing %q (after offset %d) in:\n%s", want, at, text)
		}
		at += i + len(want)
	}
	if strings.Contains(text, "- 2026-11-07 01:02 backlog") {
		t.Fatal("a no-op transition was listed in the state history")
	}
}

func TestFindIssueIgnoresASameNumberInAnotherProject(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateBacklog, 1)
	// A DEC issue carries its task's hk ID; it must not be adopted as the task.
	yt.issues["2-1"] = Issue{ID: "2-1", IDReadable: "DEC-1", Project: &ProjectRef{ID: "0-3", ShortName: "DEC"}, CustomFields: []IssueField{
		{Name: "hk ID", Value: json.RawMessage(itoa(task.ID))},
	}}
	svc := newTestService(t, yt, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if yt.createCalls != 1 {
		t.Fatalf("createCalls=%d: the DEC issue was treated as the task's", yt.createCalls)
	}
	if fs.mirrors[task.ID].IssueID == "2-1" {
		t.Fatal("the task was mapped onto a DEC issue")
	}
}

func TestAdoptionMapsPythonIssuesWithoutDuplicates(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateBacklog, 1)
	fs.comments[task.ID] = []store.TaskComment{
		{ID: 5, TaskID: task.ID, Body: "운영자 메모", Author: "operator:robin", CreatedAt: time.Now().UTC()},
		{ID: 6, TaskID: task.ID, Body: "새 메모", Author: "worker", CreatedAt: time.Now().UTC()},
	}
	yt.issues["1-1"] = Issue{ID: "1-1", IDReadable: "HK-1", Project: &ProjectRef{ID: "0-1", ShortName: "HK"}, CustomFields: []IssueField{
		{Name: "hk ID", Value: json.RawMessage(itoa(task.ID))},
	}}
	svc := newTestService(t, yt, fs)
	state := AdoptState{Issues: map[string]string{itoa(task.ID): "1-1"}, Comments: map[string][]int64{itoa(task.ID): {5}}}

	dry, err := svc.Adopt(context.Background(), state, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Actions) != 1 || dry.Actions[0].Action != "mapped" || dry.Actions[0].Comments != 1 {
		t.Fatalf("dry-run mapping=%+v", dry.Actions)
	}
	if _, ok := fs.mirrors[task.ID]; ok {
		t.Fatal("dry-run wrote a mirror row")
	}

	if _, err := svc.Adopt(context.Background(), state, false); err != nil {
		t.Fatal(err)
	}
	if fs.mirrors[task.ID].IssueID != "1-1" {
		t.Fatalf("mirror=%+v", fs.mirrors[task.ID])
	}
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if yt.createCalls != 0 {
		t.Fatalf("an adopted issue was created again: createCalls=%d", yt.createCalls)
	}
	if len(yt.comments) != 1 || !strings.Contains(yt.comments[0], "새 메모") {
		t.Fatalf("only the new comment should be posted: %v", yt.comments)
	}

	updates := len(yt.issueUpdates)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(yt.issueUpdates) != updates || len(yt.comments) != 1 {
		t.Fatalf("a second run wrote again: updates %d→%d comments=%d", updates, len(yt.issueUpdates), len(yt.comments))
	}
}

func TestStalledFlagIsSetAndCleared(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateDeveloping, 1)
	fs.stalled[task.ID] = true
	svc := newTestService(t, yt, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	created := yt.issues[fs.mirrors[task.ID].IssueID]
	if f, ok := IssueFieldByName(created.CustomFields, "멈춤"); !ok || !strings.Contains(string(f.Value), "멈춤") {
		t.Fatalf("stalled task was not flagged: %+v", created.CustomFields)
	}
	// The task moves on: the flag must be cleared with an explicit null.
	delete(fs.stalled, task.ID)
	fs.mu.Lock()
	cur := fs.tasks[task.ID]
	cur.UpdatedAt = cur.UpdatedAt.Add(time.Minute)
	fs.tasks[task.ID] = cur
	fs.mu.Unlock()
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	last := yt.issueUpdates[len(yt.issueUpdates)-1]
	if _, ok := last["customFields"]; !ok {
		t.Fatalf("no field write: %v", last)
	}
	fields, _ := last["customFields"].([]any)
	for _, raw := range fields {
		field, _ := raw.(map[string]any)
		if field["name"] == "멈춤" && field["value"] != nil {
			t.Fatalf("멈춤 was not cleared: %v", field)
		}
	}
}

func TestInFlightTaskFollowsItsPullRequest(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateDeveloping, 1)
	now := time.Now().UTC()
	head := "abc1234def"
	fs.changes[task.ID] = []store.Change{{ID: 1, Repo: "o/r", PRNumber: 7, PR: "https://github.com/o/r/pull/7", State: "open", OpenedAt: now, HeadSHA: ptr(head)}}
	fs.changeStates["o/r#7"] = store.ChangeView{
		Change:   store.ChangeSummary{Repo: "o/r", PRNumber: 7, PR: "https://github.com/o/r/pull/7", State: "open", Title: ptr("PR title"), HeadSHA: ptr(head)},
		Gate:     &store.ChangeGate{State: "blocked", Reasons: []string{"checks"}},
		Checks:   &[]store.ChangeCheck{{HeadSHA: head, Name: "ci-test", State: "completed", Conclusion: ptr("failure")}},
		Findings: &[]store.ChangeFinding{{ReviewedHead: head, Status: "open", Severity: ptr("high")}, {ReviewedHead: head, Status: "open", Severity: ptr("low")}},
	}
	svc := newTestService(t, yt, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	ref := fs.mirrors[task.ID]
	fs.mu.Unlock()
	issue := yt.issues[ref.IssueID]
	if !strings.Contains(issue.Description, "- 실패한 CI: ci-test") || !strings.Contains(issue.Description, "- 열린 리뷰 지적: 2건 (높음 이상 1건)") {
		t.Fatalf("PR section missing:\n%s", issue.Description)
	}
	// Nothing on the task changed, but the PR did: an in-flight task is pushed again.
	before := len(yt.issueUpdates)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(yt.issueUpdates) != before+1 {
		t.Fatalf("an in-flight task was not pushed on the second pass: %d→%d", before, len(yt.issueUpdates))
	}
}

func TestNoiseCommentIsLinkedOnceAndNeverDue(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateBacklog, 1)
	fs.comments[task.ID] = []store.TaskComment{{ID: 11, TaskID: task.ID, Body: "spawned request=j9", Author: "worker", CreatedAt: time.Now().UTC()}}
	svc := newTestService(t, yt, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(yt.comments) != 0 {
		t.Fatalf("a machinery note was posted: %v", yt.comments)
	}
	if fs.commentLinks[11] != noiseLink(11) {
		t.Fatalf("note not marked handled: %v", fs.commentLinks)
	}
}

func TestDecisionIssuesAreCreatedOnceAndRerunIsIdempotent(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	// A backlog task: an in-flight task is rewritten on every pass, which
	// would hide whether the decision issue was rewritten.
	task := fs.addTask("handoffkeep", "handoffkeep", "english", store.StateBacklog, 1)
	due := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	fs.decisions = []store.DecisionRecord{{
		ID: 7, TaskID: &task.ID, SourceRef: "dr-1", Lane: "handoffkeep", Question: "Use the new schema?",
		Options:     &store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Yes", Recommended: true}, {Key: "B", Label: "No"}}, AllowFree: true},
		Recommended: "A", DefaultAction: "B", DueAt: &due, Status: store.DecisionStatusOpen, UpdatedAt: time.Now().UTC(),
	}}
	svc := newTestService(t, yt, fs)
	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if len(report.Decisions) != 1 || report.Decisions[0].Action != "create" {
		t.Fatalf("decisions=%+v", report.Decisions)
	}
	var dec Issue
	for _, issue := range yt.issues {
		if issue.Project != nil && issue.Project.ShortName == DecisionProject {
			dec = issue
		}
	}
	if dec.ID == "" {
		t.Fatal("no DEC issue was created")
	}
	if dec.Summary != "[결정 7] Use the new schema?" {
		t.Fatalf("summary=%q", dec.Summary)
	}
	for _, want := range []string{"## 질문\nUse the new schema?", "- **A** Yes ✓ 권장", "- **B** No", "- 직접 답 가능", "답이 없으면: B", "기한: 2026-11-09 00:00"} {
		if !strings.Contains(dec.Description, want) {
			t.Fatalf("DEC body missing %q:\n%s", want, dec.Description)
		}
	}

	creates, updates := yt.createCalls, len(yt.issueUpdates)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if yt.createCalls != creates || len(yt.issueUpdates) != updates {
		t.Fatalf("re-run wrote again: creates %d→%d updates %d→%d", creates, yt.createCalls, updates, len(yt.issueUpdates))
	}
}

func TestDryRunWritesNothingOnFixtures(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "English title (grade B, T2)", store.StateBacklog, 3)
	fs.decisions = []store.DecisionRecord{{ID: 8, Lane: "handoffkeep", Question: "Q?", Status: store.DecisionStatusOpen, UpdatedAt: time.Now().UTC(), TaskID: &task.ID}}
	svc := newTestService(t, yt, fs)
	report, err := svc.Sync(context.Background(), SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if yt.createCalls != 0 || len(yt.issueUpdates) != 0 || len(yt.comments) != 0 || len(yt.savedQueries) != 0 {
		t.Fatalf("dry-run wrote: creates=%d updates=%d comments=%d saved=%d", yt.createCalls, len(yt.issueUpdates), len(yt.comments), len(yt.savedQueries))
	}
	if len(fs.decisionMirrors) != 0 || len(fs.mirrors) != 0 {
		t.Fatalf("dry-run wrote hk rows: decisions=%d mirrors=%d", len(fs.decisionMirrors), len(fs.mirrors))
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("dry-run report:\n%s", out)
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDecisionIssueShowsTheDisplayTemplateNotTheStoredJSON(t *testing.T) {
	fs := newFakeStore()
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "deploy", store.StateBacklog, 1)
	question := `Approve deployment {"identity":{"environment_known":true,"config":"c","payload":"p"},"host":"h1","repo":"r","previous":"unknown","release":"abcdef1234","paths":[],"reasons":[]}`
	options := &store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}
	request := store.DecisionRequest{ID: "dr-1", Question: question, RequestedBy: "mergewake-design", Status: store.DecisionRequestOpen}
	fs.mu.Lock()
	cur := fs.tasks[task.ID]
	cur.Refs.DecisionRequest = &request
	cur.Refs.DecisionOptions = options
	fs.tasks[task.ID] = cur
	fs.mu.Unlock()
	fs.decisions = []store.DecisionRecord{{ID: 9, TaskID: &task.ID, SourceRef: "dr-1", Lane: "handoffkeep", Question: question, Options: options, Status: store.DecisionStatusOpen, UpdatedAt: time.Now().UTC()}}
	svc := newTestService(t, yt, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	var body string
	for _, issue := range yt.issues {
		if issue.Project != nil && issue.Project.ShortName == DecisionProject {
			body = issue.Description
		}
	}
	if strings.Count(body, "이 배포 승인") != 1 {
		t.Fatalf("a choice is listed twice:\n%s", body)
	}
	if strings.Contains(body, `"identity"`) || !strings.Contains(body, "이 배포 승인") {
		t.Fatalf("the DEC body shows the stored JSON or the stored labels:\n%s", body)
	}
	// The stored options and question are untouched: the template is a read-time view.
	if options.Options[0].Label != "Approve this deployment" || fs.decisions[0].Question != question {
		t.Fatal("the rendering changed the stored text")
	}
}

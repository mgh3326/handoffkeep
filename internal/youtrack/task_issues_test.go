package youtrack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const taskIssueJSON = `{"id":"2-17","idReadable":"HKW-17","summary":"#4430 console reads YouTrack (grade A, T2)",
	"created":1762905600000,"updated":1762909200000,"resolved":null,"project":{"id":"0-5","shortName":"HKW"},
	"customFields":[
		{"name":"hk ID","$type":"SimpleIssueCustomField","value":4430},
		{"name":"hk 상태","$type":"StateIssueCustomField","value":{"name":"개발 중"}},
		{"name":"레인","$type":"SingleEnumIssueCustomField","value":{"name":"handoffkeep"}},
		{"name":"종류","$type":"SingleEnumIssueCustomField","value":{"name":"implement"}},
		{"name":"등급","$type":"SingleEnumIssueCustomField","value":{"name":"A"}},
		{"name":"티어","$type":"SingleEnumIssueCustomField","value":{"name":"T2"}},
		{"name":"hk 우선순위","$type":"SimpleIssueCustomField","value":3},
		{"name":"hk 프로젝트","$type":"TextIssueCustomField","value":{"text":"handoffkeep"}},
		{"name":"영역","$type":"TextIssueCustomField","value":{"text":"console"}},
		{"name":"PR","$type":"TextIssueCustomField","value":{"text":"https://github.com/o/r/pull/1"}},
		{"name":"작업 job","$type":"TextIssueCustomField","value":{"text":"hk4430-console-yt"}},
		{"name":"멈춤","$type":"MultiEnumIssueCustomField","value":[{"name":"멈춤"}]}
	]}`

func TestTaskFromIssueReadsEveryConsoleField(t *testing.T) {
	var issue Issue
	if err := json.Unmarshal([]byte(taskIssueJSON), &issue); err != nil {
		t.Fatal(err)
	}
	got := TaskFromIssue(DefaultSchema(), issue)
	want := TaskIssue{TaskID: 4430, IssueID: "2-17", IssueReadable: "HKW-17", ProjectShort: "HKW",
		Title: "console reads YouTrack (grade A, T2)", State: "developing", StateKnown: true, Lane: "handoffkeep",
		Kind: "implement", Grade: "A", Tier: "T2", Priority: 3, Project: "handoffkeep", Area: "console",
		PR: "https://github.com/o/r/pull/1", Job: "hk4430-console-yt", Stalled: true,
		Created: time.UnixMilli(1762905600000).UTC(), Updated: time.UnixMilli(1762909200000).UTC()}
	if got != want {
		t.Fatalf("task=%+v\nwant %+v", got, want)
	}
}

func TestTaskFromIssueKeepsUnknownStateAndUnassignedProject(t *testing.T) {
	resolved := int64(1762909200000)
	issue := Issue{ID: "2-1", Summary: "#9 other title", Resolved: &resolved, CustomFields: []IssueField{
		{Name: "hk ID", Value: json.RawMessage(`8`)},
		{Name: "hk 상태", Value: json.RawMessage(`{"name":"새 상태"}`)},
		{Name: "hk 프로젝트", Value: json.RawMessage(`{"text":"Unassigned"}`)},
		{Name: "멈춤", Value: json.RawMessage(`[]`)},
	}}
	got := TaskFromIssue(DefaultSchema(), issue)
	// The prefix names another id, so the summary is kept whole.
	if got.TaskID != 8 || got.Title != "#9 other title" || got.State != "새 상태" || got.StateKnown || got.Project != "" || got.Stalled {
		t.Fatalf("task=%+v", got)
	}
	if got.Resolved == nil || !got.Resolved.Equal(time.UnixMilli(resolved)) {
		t.Fatalf("resolved=%v", got.Resolved)
	}
}

func TestTaskIssuesQuery(t *testing.T) {
	got := TaskIssuesQuery([]string{"HK", "HKW", "", "HK", "BAD}"})
	want := "(project: {HK} or project: {HKW}) sort by: created asc"
	if got != want {
		t.Fatalf("query=%q want %q", got, want)
	}
	if TaskIssuesQuery(nil) != "" {
		t.Fatal("no projects must give no query")
	}
}

func TestListTaskIssuesAsksForOnePageOfConsoleFields(t *testing.T) {
	var seen *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_, _ = w.Write([]byte("[" + taskIssueJSON + "]"))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	issues, err := c.ListTaskIssues(context.Background(), "project: {HK}", 500, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].IDReadable != "HKW-17" || issues[0].Created != 1762905600000 {
		t.Fatalf("issues=%+v", issues)
	}
	q := seen.URL.Query()
	if seen.URL.Path != "/api/issues" || q.Get("query") != "project: {HK}" || q.Get("$skip") != "500" || q.Get("$top") != "500" || q.Get("fields") != taskIssueFields {
		t.Fatalf("request=%s", seen.URL.String())
	}
	if seen.Header.Get("Authorization") != "Bearer fixture-token" {
		t.Fatal("bot token not sent as bearer")
	}
}

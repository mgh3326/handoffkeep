package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// searchServer serves /v1/tasks/search with body, recording the query.
func searchServer(t *testing.T, body string, queries *[]string, status int) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/search" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*queries = append(*queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(h.Close)
	t.Setenv("HANDOFFKEEP_URL", h.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	return h
}

// TestTasksSearchCLI proves tasks search finds words across states, sends
// the project/state/lane/limit filters, and exits 0 with an empty list.
func TestTasksSearchCLI(t *testing.T) {
	const body = `{"tasks":[
{"id":3,"lane":"d","title":"probe alpha","kind":"implement","state":"backlog","created_by":"t","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z","project":"wrk"},
{"id":7,"lane":"d","title":"probe beta","kind":"implement","state":"developing","created_by":"t","created_at":"2026-11-02T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","project":"wrk"},
{"id":9,"lane":"e","title":"probe gamma","kind":"implement","state":"reviewing","created_by":"t","created_at":"2026-11-03T00:00:00Z","updated_at":"2026-11-03T00:00:00Z","project":"other"}
],"truncated":false}` + "\n"
	var queries []string
	searchServer(t, body, &queries, 0)

	// Words find tasks in several states; flags ride along or not at all.
	var out bytes.Buffer
	if err := tasksCmd([]string{"search", "probe"}, &out); err != nil {
		t.Fatal(err)
	}
	var printed struct {
		Tasks []struct {
			ID      int64   `json:"id"`
			State   string  `json:"state"`
			Lane    string  `json:"lane"`
			Project *string `json:"project"`
			Title   string  `json:"title"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(printed.Tasks) != 3 {
		t.Fatalf("tasks=%v", printed.Tasks)
	}
	states := map[string]bool{}
	for _, x := range printed.Tasks {
		states[x.State] = true
		if x.Title == "" || x.Lane == "" || x.Project == nil {
			t.Fatalf("task %d misses shown fields: %+v", x.ID, x)
		}
	}
	for _, s := range []string{"backlog", "developing", "reviewing"} {
		if !states[s] {
			t.Fatalf("state %s missing in %v", s, states)
		}
	}
	values, err := url.ParseQuery(queries[0])
	if err != nil || values.Get("q") != "probe" || values.Get("limit") != "20" {
		t.Fatalf("query=%q err=%v", queries[0], err)
	}
	if values.Get("state") != "" || values.Get("project") != "" || values.Has("open") {
		t.Fatalf("unset filters leaked: %q", queries[0])
	}

	// The project, state, lane and limit filters narrow the request.
	out.Reset()
	if err := tasksCmd([]string{"search", "probe", "words", "--state", "developing", "--project", "wrk", "--lane", "d", "--limit", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries=%v", queries)
	}
	values, err = url.ParseQuery(queries[1])
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"q": "probe words", "state": "developing", "project": "wrk", "lane": "d", "limit": "5"} {
		if got := values.Get(key); got != want {
			t.Fatalf("query %s=%q want %q", key, got, want)
		}
	}

	// --open reaches the route; --state and --open refuse together.
	out.Reset()
	if err := tasksCmd([]string{"search", "probe", "--open"}, &out); err != nil {
		t.Fatal(err)
	}
	values, err = url.ParseQuery(queries[2])
	if err != nil || values.Get("open") != "true" {
		t.Fatalf("open query=%q err=%v", queries[2], err)
	}
	if err := tasksCmd([]string{"search", "probe", "--open", "--state", "backlog"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--open and --state are exclusive") {
		t.Fatalf("open+state err=%v", err)
	}
	for _, args := range [][]string{
		{"search", "probe", "--limit", "0"},
		{"search", "probe", "--limit", "51"},
	} {
		if err := tasksCmd(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--limit must be between 1 and 50") {
			t.Fatalf("%v err=%v", args, err)
		}
	}
	if err := tasksCmd([]string{"search"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "tasks search <words") {
		t.Fatalf("no words err=%v", err)
	}
}

// TestTasksSearchCLIEmptyResult proves no match exits 0 with an empty list.
func TestTasksSearchCLIEmptyResult(t *testing.T) {
	var queries []string
	searchServer(t, `{"tasks":[],"truncated":false}`+"\n", &queries, 0)
	var out bytes.Buffer
	if err := tasksCmd([]string{"search", "nothing-here"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"tasks":[]}` {
		t.Fatalf("output=%q", out.String())
	}
}

// TestTasksSearchCLISurfacesParamDetail proves a server refusal naming its
// parameter reaches the user verbatim.
func TestTasksSearchCLISurfacesParamDetail(t *testing.T) {
	var queries []string
	searchServer(t, `{"error":"invalid_context","param":"limit","allowed":"1..1000"}`+"\n", &queries, http.StatusBadRequest)
	err := tasksCmd([]string{"search", "probe", "--limit", "7"}, &bytes.Buffer{})
	if err == nil || err.Error() != "invalid_context: limit must be 1..1000" {
		t.Fatalf("err=%v", err)
	}
}

// TestTasksHelpPrintsUsage proves tasks list -h and tasks search -h print
// the usage with limits and allowed values instead of flag: help requested.
func TestTasksHelpPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := tasksCmd([]string{"list", "-h"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tasks list", "1..1000", "backlog", "true or false"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list help misses %q in %q", want, out.String())
		}
	}
	out.Reset()
	if err := tasksCmd([]string{"search", "-h"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tasks search <words", "1..50", "backlog", "--open"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("search help misses %q in %q", want, out.String())
		}
	}
}

// TestTasksSearchCLITruncatedSurfaces proves a window the server cut prints
// "truncated": true, so a cut search cannot read as "no duplicate".
func TestTasksSearchCLITruncatedSurfaces(t *testing.T) {
	var queries []string
	searchServer(t, `{"tasks":[{"id":3,"lane":"d","title":"probe alpha","kind":"implement","state":"backlog","created_by":"t","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}],"truncated":true}`+"\n", &queries, 0)
	var out bytes.Buffer
	if err := tasksCmd([]string{"search", "probe"}, &out); err != nil {
		t.Fatal(err)
	}
	var printed struct {
		Tasks     []json.RawMessage `json:"tasks"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !printed.Truncated || len(printed.Tasks) != 1 {
		t.Fatalf("output=%q", out.String())
	}
}

// TestTasksSearchCLIOpenFalseWithState proves --open=false is not an
// exclusive conflict with --state, while --open with --state still refuses.
func TestTasksSearchCLIOpenFalseWithState(t *testing.T) {
	var queries []string
	searchServer(t, `{"tasks":[],"truncated":false}`+"\n", &queries, 0)
	if err := tasksCmd([]string{"search", "probe", "--open=false", "--state", "backlog"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("--open=false --state refused: %v", err)
	}
	values, err := url.ParseQuery(queries[0])
	if err != nil || values.Get("state") != "backlog" || values.Has("open") {
		t.Fatalf("query=%q err=%v", queries[0], err)
	}
	if err := tasksCmd([]string{"search", "probe", "--open", "--state", "backlog"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--open and --state are exclusive") {
		t.Fatalf("--open --state err=%v", err)
	}
}

// TestTasksListHelpAdvertisesOnlyRealFlags proves tasks list -h names no
// flag the CLI does not define (--change and --after-id were advertised but
// never wired).
func TestTasksListHelpAdvertisesOnlyRealFlags(t *testing.T) {
	var out bytes.Buffer
	if err := tasksCmd([]string{"list", "-h"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"--change", "--after-id"} {
		if strings.Contains(out.String(), gone) {
			t.Fatalf("list help still advertises %s: %q", gone, out.String())
		}
	}
}

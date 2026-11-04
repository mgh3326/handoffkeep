package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func cliEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", "")
}

func TestTasksEventCLI(t *testing.T) {
	cliEnv(t)
	const pr = "https://github.com/example/app/pull/7"
	var got []store.TaskSourceEvent
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/task-events" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var in store.TaskSourceEvent
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		got = append(got, in)
		w.Header().Set("Content-Type", "application/json")
		switch in.TaskID {
		case 1:
			_, _ = io.WriteString(w, `{"applied":true,"redelivered":false,"state":"merged","task":{"id":1,"state":"merged"}}`)
		case 2:
			_, _ = io.WriteString(w, `{"applied":false,"redelivered":false,"state":"dropped","reason":"the task is canceled","task":{"id":2}}`)
		case 3:
			_, _ = io.WriteString(w, `{"applied":false,"redelivered":true,"state":"merged","task":{"id":3}}`)
		case 4:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_context"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found"}`)
		}
	}))
	defer h.Close()
	cli := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(append(append([]string{"tasks", "event"}, args...), "--url", h.URL, "--token", "tok"), &out, io.Discard)
		return out.String(), err
	}
	out, err := cli("1", "--source", "mergewake.merge", "--source-ref", pr, "--at", "2026-11-05T01:02:03Z")
	if err != nil || !strings.Contains(out, `"applied":true`) {
		t.Fatalf("applied: %s %v", out, err)
	}
	if got[0].Source != "mergewake.merge" || got[0].SourceRef != pr || got[0].TaskID != 1 || got[0].At == nil || !got[0].At.Equal(time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("request %+v", got[0])
	}
	// Single-dash flags after the task id keep their values.
	if out, err := cli("1", "-source", "mergewake.merge", "-source-ref", pr); err != nil || !strings.Contains(out, `"applied":true`) {
		t.Fatalf("single dash: %s %v", out, err)
	}
	if last := got[len(got)-1]; last.Source != "mergewake.merge" || last.SourceRef != pr || last.TaskID != 1 {
		t.Fatalf("single dash request %+v", last)
	}
	got = got[:1]
	if out, err = cli("2", "--source", "mergewake.merge", "--source-ref", pr); err != nil || !strings.Contains(out, `"applied":false`) || !strings.Contains(out, "canceled") {
		t.Fatalf("not applied: %s %v", out, err)
	}
	if out, err = cli("3", "--source", "mergewake.merge", "--source-ref", pr); err != nil || !strings.Contains(out, `"redelivered":true`) {
		t.Fatalf("redelivered: %s %v", out, err)
	}
	if _, err = cli("4", "--source", "mergewake.closed", "--source-ref", pr); err == nil || !strings.Contains(err.Error(), "invalid_context") {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err = cli("5", "--source", "mergewake.merge", "--source-ref", pr); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("missing task: %v", err)
	}
	// A revert carries the reverted PR.
	if _, err = cli("1", "--source", "mergewake.revert", "--source-ref", pr+"1", "--reverted-pr", pr); err != nil || got[len(got)-1].RevertedPR != pr {
		t.Fatalf("revert: %+v %v", got[len(got)-1], err)
	}
	// Refused before any request.
	n := len(got)
	for _, args := range [][]string{{"--source", "mergewake.merge", "--source-ref", pr}, {"1", "--source-ref", pr}, {"1", "--source", "mergewake.merge", "--source-ref", pr, "--at", "yesterday"}} {
		if _, err = cli(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if len(got) != n {
		t.Fatalf("refused commands sent %d requests", len(got)-n)
	}
}

func TestTasksSweepCLIIsDryRunOnly(t *testing.T) {
	cliEnv(t)
	var paths []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"days":14,"dry_run":true,"tasks":[{"id":3,"lane":"l","title":"idle","idle_since":"2026-10-01T00:00:00Z"}]}`)
	}))
	defer h.Close()
	var out bytes.Buffer
	if err := run([]string{"tasks", "sweep", "--dry-run", "--days", "14", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "GET /v1/tasks/sweep?days=14" || !strings.Contains(out.String(), `"count":1`) {
		t.Fatalf("paths=%v out=%s", paths, out.String())
	}
	for _, args := range [][]string{{"--days", "14"}, {"--dry-run"}, {"--dry-run", "--days", "0"}} {
		if err := run(append(append([]string{"tasks", "sweep"}, args...), "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if len(paths) != 1 {
		t.Fatalf("refused commands sent requests: %v", paths)
	}
}

func TestTasksListStalledCLI(t *testing.T) {
	cliEnv(t)
	var query string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"tasks":[{"id":1,"state":"developing","stalled":true}],"stalled":true}`)
	}))
	defer h.Close()
	var out bytes.Buffer
	if err := run([]string{"tasks", "list", "--stalled", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "stalled=true") || !strings.Contains(out.String(), `"stalled":true`) {
		t.Fatalf("query=%s out=%s", query, out.String())
	}
	if err := run([]string{"tasks", "claim", "1", "--stalled", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err == nil {
		t.Fatal("--stalled accepted outside tasks list")
	}
}

func TestBacklogSweepWorkerOffByDefault(t *testing.T) {
	// days 0 returns before the store is touched: a zero Store has no pool
	// and would panic if the sweep ran.
	done := make(chan struct{})
	go func() {
		backlogSweepWorker{store: &store.Store{}}.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep worker runs with backlog_icebox_days unset")
	}
}

func TestServeAutomationFlagBounds(t *testing.T) {
	for _, tc := range []struct {
		hours, days int
		ok          bool
	}{
		{1, 0, true}, {8760, 3650, true}, {24, 1, true},
		{0, 0, false}, {8761, 0, false}, {2562048, 0, false},
		{24, -1, false}, {24, 3651, false}, {24, 106752, false},
	} {
		if err := validAutomationFlags(tc.hours, tc.days); (err == nil) != tc.ok {
			t.Fatalf("hours=%d days=%d: err=%v", tc.hours, tc.days, err)
		}
	}
	// serve refuses out-of-range values at startup, before it reads any
	// environment or opens the store.
	for _, tc := range []struct{ args []string }{
		{[]string{"--started-stalled-hours", "8761"}},
		{[]string{"--started-stalled-hours", "0"}},
		{[]string{"--backlog-icebox-days", "3651"}},
		{[]string{"--backlog-icebox-days", "106752"}},
		{[]string{"--backlog-icebox-days", "-1"}},
	} {
		err := serve(tc.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.args[0]) {
			t.Fatalf("serve %v: err=%v", tc.args, err)
		}
	}
}

func TestTasksSweepCLIRefusesOutOfRangeDays(t *testing.T) {
	cliEnv(t)
	hits := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer h.Close()
	for _, days := range []string{"0", "3651", "106752"} {
		err := run([]string{"tasks", "sweep", "--dry-run", "--days", days, "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "1..3650") {
			t.Fatalf("days=%s: err=%v", days, err)
		}
	}
	if hits != 0 {
		t.Fatalf("the CLI called the server %d times for refused days", hits)
	}
}

func TestSplitTaskArgsSingleDashValueFlags(t *testing.T) {
	valueFlags := map[string]bool{"--source": true, "--source-ref": true}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"7", "-source", "mergewake.merge", "-source-ref", "u"}, []string{"-source", "mergewake.merge", "-source-ref", "u", "7"}},
		{[]string{"7", "--source", "mergewake.merge", "--source-ref=u"}, []string{"--source", "mergewake.merge", "--source-ref=u", "7"}},
		{[]string{"-source=mergewake.merge", "7", "-source-ref", "u"}, []string{"-source=mergewake.merge", "-source-ref", "u", "7"}},
	} {
		got, err := splitTaskArgs(tc.args, valueFlags)
		if err != nil || strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Fatalf("%v -> %v err=%v, want %v", tc.args, got, err, tc.want)
		}
	}
	if _, err := splitTaskArgs([]string{"7", "-source"}, valueFlags); err == nil {
		t.Fatal("a trailing -source without a value was accepted")
	}
}

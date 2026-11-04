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

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

const ledgerFixture = `{
 "consumer":"desk","at":"2026-11-05T12:00:00Z","stalled_after_hours":24,
 "in_flight":{
  "projects":[{"project":"experiment","started":2,"health":"at_risk","last_update_at":"2026-10-25T12:00:00Z","stale":true}],
  "tasks":[{"id":11,"lane":"lane-a","title":"fresh work","kind":"implement","state":"developing","priority":5,"project":"experiment",
   "activity_at":"2026-11-05T10:00:00Z","last_event":{"kind":"transition","source":"","at":"2026-11-05T10:00:00Z"},"next_trigger":"review","next_source":"mergewake.pr"}]},
 "waiting_on_operator":{"count":2,"groups":[
  {"task_id":12,"task":{"id":12,"lane":"lane-a","title":"flagged work","kind":"implement","state":"reviewing","priority":5,"project":"experiment"},
   "requests":[{"id":5,"source":"task_flag","source_ref":"12/9","task_id":12,"question":"ship it?","status":"open","overdue":true,"created_at":"2026-11-05T09:00:00Z"}]},
  {"task_id":null,"task":null,"requests":[{"id":6,"source":"chat","source_ref":"Q-1","task_id":null,"question":"which host?","status":"open","created_at":"2026-11-04T12:00:00Z"}]}]},
 "stalled":{
  "tasks":[{"id":13,"lane":"lane-a","title":"idle work","kind":"implement","state":"developing","priority":5,"project":"experiment",
   "activity_at":"2026-11-02T12:00:00Z","last_event":null,"next_trigger":"review","next_source":"mergewake.pr"}],
  "events":[{"event_id":88,"task":{"id":14,"lane":"lane-a","title":"dropped work","kind":"implement","state":"dropped","priority":5,"project":"experiment"},
   "source":"mergewake.merge","source_ref":"https://github.com/example/app/pull/7","note":"not applied","at":"2026-11-05T11:00:00Z"}]},
 "triage":{"count":1,"oldest_at":"2026-11-02T00:00:00Z","oldest_age_seconds":302400,
  "tasks":[{"id":15,"lane":"lane-b","title":"new intake","kind":"implement","state":"triage","priority":0,"project":null,"since":"2026-11-02T00:00:00Z","age_seconds":302400}]},
 "changed_since_ack":{"no_cursor":false,"cursor":{"consumer":"desk","acked_sync_id":100,"acked_at":"2026-11-05T08:00:00Z","updated_by":"tester"},
  "max_sync_id":103,"last_sync_id":103,"total":3,"truncated":false,
  "summary":[{"model":"task","action":"I","count":1},{"model":"task","action":"U","count":2}],
  "rows":[{"sync_id":101,"model":"task","model_id":"11","action":"I","at":"2026-11-05T09:00:00Z"},
   {"sync_id":102,"model":"task","model_id":"11","action":"U","at":"2026-11-05T09:30:00Z"},
   {"sync_id":103,"model":"task","model_id":"12","action":"U","at":"2026-11-05T10:00:00Z"}]}
}`

type ledgerCLICall struct {
	Method, Path, Query, Body string
}

func ledgerCLIServer(t *testing.T, status int, response string) (*httptest.Server, *[]ledgerCLICall) {
	t.Helper()
	calls := []ledgerCLICall{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls = append(calls, ledgerCLICall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(raw)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &calls
}

func TestLedgerCLITextSectionsInOrder(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, ledgerFixture)
	var out bytes.Buffer
	if err := run([]string{"ledger", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "GET" || c.Path != "/v1/ledger" || c.Query != "consumer=desk" {
		t.Fatalf("call=%+v", c)
	}
	text := out.String()
	last := -1
	for _, want := range []string{
		"Ledger for desk at 2026-11-05T12:00:00Z (stalled after 24h)",
		"1. In flight (1)",
		"project experiment: 2 started, at_risk 11d0h ago (stale)",
		"#11 developing [experiment] fresh work — last transition 2h0m ago; next: review (mergewake.pr)",
		"2. Waiting on operator (2)",
		"#12 reviewing flagged work",
		"request 5 task_flag 3h0m ago overdue: ship it?",
		"no task",
		"request 6 chat 1d0h ago: which host?",
		"3. Stalled (1 tasks, 1 events)",
		"#13 developing [experiment] idle work — idle 3d0h; next: review (mergewake.pr)",
		"event 88 mergewake.merge on #14 dropped dropped work, 1h0m ago: https://github.com/example/app/pull/7",
		"4. Triage (1, oldest 3d12h)",
		"#15 [-] new intake (3d12h)",
		"5. Changed since last ack (3 changes after 100, feed at 103)",
		"task I 1",
		"task U 2",
		"all 3 rows end at 103; ack with handoffkeep ledger ack --consumer desk 103",
	} {
		i := strings.Index(text, want)
		if i < 0 {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
		if i < last {
			t.Fatalf("%q is out of order:\n%s", want, text)
		}
		last = i
	}
	if strings.Contains(text, "101 task 11 I") {
		t.Fatalf("ledger text lists feed rows:\n%s", text)
	}
}

func TestLedgerCLIJSON(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, ledgerFixture)
	var out bytes.Buffer
	if err := run([]string{"ledger", "--consumer", "desk-2", "--json", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Query != "consumer=desk-2" {
		t.Fatalf("call=%+v", c)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("out=%s err=%v", out.String(), err)
	}
	// Sections appear in their fixed order in the JSON object too.
	text := out.String()
	last := -1
	for _, key := range []string{`"in_flight"`, `"waiting_on_operator"`, `"stalled"`, `"triage"`, `"changed_since_ack"`} {
		i := strings.Index(text, key)
		if i < 0 || i < last {
			t.Fatalf("key %s missing or out of order: %s", key, text)
		}
		last = i
	}
}

func TestSinceCLI(t *testing.T) {
	h, _ := ledgerCLIServer(t, http.StatusOK, ledgerFixture)
	var out bytes.Buffer
	if err := run([]string{"since", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "In flight") || strings.Contains(text, "Triage") || !strings.HasPrefix(text, "5. Changed since last ack") {
		t.Fatalf("since prints other sections:\n%s", text)
	}
	for _, want := range []string{"101 task 11 I 2026-11-05T09:00:00Z", "103 task 12 U 2026-11-05T10:00:00Z"} {
		if !strings.Contains(text, want) {
			t.Fatalf("since lacks %q:\n%s", want, text)
		}
	}
	out.Reset()
	if err := run([]string{"since", "--json", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var ch struct {
		LastSyncID int64             `json:"last_sync_id"`
		Rows       []json.RawMessage `json:"rows"`
		InFlight   json.RawMessage   `json:"in_flight"`
	}
	if err := json.Unmarshal(out.Bytes(), &ch); err != nil || ch.LastSyncID != 103 || len(ch.Rows) != 3 || ch.InFlight != nil {
		t.Fatalf("since --json %s err=%v", out.String(), err)
	}
}

func TestLedgerCLINoCursorAndReset(t *testing.T) {
	h, _ := ledgerCLIServer(t, http.StatusOK, `{"consumer":"desk","at":"2026-11-05T12:00:00Z","changed_since_ack":{"no_cursor":true,"cursor":null,"max_sync_id":77,"summary":[],"rows":[]}}`)
	var out bytes.Buffer
	if err := run([]string{"since", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no cursor yet for desk (feed at 77)") {
		t.Fatalf("no-cursor text %q", out.String())
	}
	h, _ = ledgerCLIServer(t, http.StatusGone, `{"reset":true,"min_sync_id":500}`)
	err := run([]string{"ledger", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "oldest sync id 500") {
		t.Fatalf("reset err=%v", err)
	}
}

func TestLedgerAckCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusOK, `{"consumer":"desk","acked_sync_id":103,"acked_at":"2026-11-05T12:00:00Z","updated_by":"tester"}`)
	var out bytes.Buffer
	if err := run([]string{"ledger", "ack", "103", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "POST" || c.Path != "/v1/ledger/ack" || c.Body != `{"consumer":"desk","sync_id":103}` {
		t.Fatalf("call=%+v", c)
	}
	if !strings.Contains(out.String(), `"acked_sync_id":103`) {
		t.Fatalf("out=%s", out.String())
	}
	if err := run([]string{"ledger", "ack", "-consumer", "other", "7", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[1]; c.Body != `{"consumer":"other","sync_id":7}` {
		t.Fatalf("single-dash consumer call=%+v", c)
	}
	for _, args := range [][]string{
		{"ledger", "ack"},
		{"ledger", "ack", "x"},
		{"ledger", "ack", "1", "2"},
		{"ledger", "ack", "--consumer"},
		{"ledger", "extra"},
		{"since", "extra"},
	} {
		if err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("refused arguments reached the server: %+v", *calls)
	}
}

func TestProjectStatusCLI(t *testing.T) {
	h, calls := ledgerCLIServer(t, http.StatusCreated, `{"id":3,"project":"experiment","health":"at_risk","body":"slipping","snapshot":{"started":2},"diff":{"previous_id":2}}`)
	var out bytes.Buffer
	if err := run([]string{"tasks", "project-status", "experiment", "--health", "at_risk", "--body", "slipping", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "POST" || c.Path != "/v1/projects/experiment/status-updates" || c.Body != `{"body":"slipping","health":"at_risk"}` {
		t.Fatalf("call=%+v", c)
	}
	var x struct {
		ID     int64  `json:"id"`
		Health string `json:"health"`
	}
	if err := json.Unmarshal(out.Bytes(), &x); err != nil || x.ID != 3 || x.Health != "at_risk" {
		t.Fatalf("out=%s err=%v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"tasks", "project-status", "experiment", "--limit", "5", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[1]; c.Method != "GET" || c.Path != "/v1/projects/experiment/status-updates" || c.Query != "limit=5" {
		t.Fatalf("list call=%+v", c)
	}
	for _, args := range [][]string{
		{"tasks", "project-status"},
		{"tasks", "project-status", "experiment", "--health", "green", "--body", "x"},
		{"tasks", "project-status", "experiment", "--health", "at_risk"},
		{"tasks", "project-status", "experiment", "--body", "x"},
		{"tasks", "project-status", "experiment", "--health", "at_risk", "--body", "x", "--limit", "2"},
		{"tasks", "project-status", "a", "b"},
	} {
		if err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("refused arguments reached the server: %+v", *calls)
	}
}

func TestLedgerAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                     "0s",
		30 * time.Second:                 "30s",
		45 * time.Minute:                 "45m",
		5*time.Hour + 12*time.Minute:     "5h12m",
		3*24*time.Hour + 4*time.Hour:     "3d4h",
		24*time.Hour + 59*time.Minute:    "1d0h",
		59*time.Second + time.Nanosecond: "59s",
	} {
		if got := ledgerAge(d); got != want {
			t.Errorf("ledgerAge(%s)=%q, want %q", d, got, want)
		}
	}
}

// TestLedgerCLIAgainstServer runs ledger, since, ack and project-status
// through the real API and store.
func TestLedgerCLIAgainstServer(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ledger_cli")
	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(api.Server{Service: api.Service{Store: st}, Tokens: api.Tokens{"desk": "tok"}}.Handler())
	t.Cleanup(h.Close)
	project := "experiment"
	x, err := st.CreateTask(ctx, store.Task{Lane: "ledger-cli-lane", Title: "cli task", Kind: "implement", CreatedBy: "ledger-test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(ctx, x.ID, "needs_decision", "ledger-cli-lane", "go?", nil, ""); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(append(args, "--url", h.URL, "--token", "tok"), &out, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}

	var l store.Ledger
	if err := json.Unmarshal([]byte(cli("ledger", "--json")), &l); err != nil || l.Consumer != "desk" || !l.ChangedSinceAck.NoCursor || l.WaitingOnOperator.Count != 1 {
		t.Fatalf("ledger --json %+v err=%v", l, err)
	}
	if text := cli("ledger"); !strings.Contains(text, "2. Waiting on operator (1)") || !strings.Contains(text, "no cursor yet for desk") {
		t.Fatalf("ledger text:\n%s", text)
	}
	var cur store.LedgerCursor
	if err := json.Unmarshal([]byte(cli("ledger", "ack", "0")), &cur); err != nil || cur.AckedSyncID != l.ChangedSinceAck.MaxSyncID || cur.UpdatedBy != "desk" {
		t.Fatalf("ack %+v err=%v", cur, err)
	}
	if _, err = st.CreateTaskComment(ctx, x.ID, "ledger-test", "progress"); err != nil {
		t.Fatal(err)
	}
	var ch store.LedgerChanges
	if err := json.Unmarshal([]byte(cli("since", "--json")), &ch); err != nil || ch.Cursor == nil || ch.Total < 1 || len(ch.Rows) < 1 || ch.Rows[0].Model != "task_comment" {
		t.Fatalf("since --json %+v err=%v", ch, err)
	}
	if text := cli("since"); !strings.Contains(text, "task_comment I 1") {
		t.Fatalf("since text:\n%s", text)
	}
	err = run([]string{"ledger", "ack", "0", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ledger_ack_backward") {
		t.Fatalf("backward ack err=%v", err)
	}

	var up store.StatusUpdate
	if err := json.Unmarshal([]byte(cli("tasks", "project-status", "experiment", "--health", "off_track", "--body", "blocked")), &up); err != nil || up.Snapshot.Backlog != 1 || up.Health != "off_track" {
		t.Fatalf("project-status %+v err=%v", up, err)
	}
	var list store.StatusUpdateList
	if err := json.Unmarshal([]byte(cli("tasks", "project-status", "experiment")), &list); err != nil || len(list.Updates) != 1 || list.Stale {
		t.Fatalf("project-status list %+v err=%v", list, err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type decisionCLICall struct {
	Method, Path, Query string
	Body                map[string]any
}

func decisionCLIServer(t *testing.T, response string) (*httptest.Server, *[]decisionCLICall) {
	t.Helper()
	calls := []decisionCLICall{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decisionCLICall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		_ = json.NewDecoder(r.Body).Decode(&c.Body)
		calls = append(calls, c)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &calls
}

func TestDecisionsListCLI(t *testing.T) {
	h, calls := decisionCLIServer(t, `{"server_time":"2026-11-05T00:00:00Z","decision_requests":[{"id":3,"source":"chat","source_ref":"Q-20261105-01","status":"open"}]}`)
	var out bytes.Buffer
	if err := run([]string{"decisions", "list", "--status", "answered", "--lane", "desk-lane", "--source", "chat", "--include-snoozed", "--limit", "9", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil || len(printed["decision_requests"].([]any)) != 1 {
		t.Fatalf("out=%s err=%v", out.String(), err)
	}
	if c := (*calls)[0]; c.Method != "GET" || c.Path != "/v1/decision-requests" || c.Query != "include_snoozed=true&lane=desk-lane&limit=9&source=chat&status=answered" {
		t.Fatalf("call=%+v", c)
	}
	if err := run([]string{"decisions", "list", "extra", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "usage: decisions list") {
		t.Fatalf("positional arg err=%v", err)
	}
}

func TestDecisionsSnoozeCLI(t *testing.T) {
	h, calls := decisionCLIServer(t, `{"id":3,"status":"open","snoozed":true}`)
	var out bytes.Buffer
	if err := run([]string{"decisions", "snooze", "--until", "2030-01-02T03:04:05+09:00", "3", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "POST" || c.Path != "/v1/decision-requests/3/snooze" || c.Body["until"] != "2030-01-01T18:04:05Z" {
		t.Fatalf("call=%+v", c)
	}
	for _, args := range [][]string{
		{"decisions", "snooze", "3"},
		{"decisions", "snooze", "--until", "tomorrow", "3"},
		{"decisions", "snooze", "--until", "2030-01-02T03:04:05Z", "0"},
		{"decisions", "snooze", "--until", "2030-01-02T03:04:05Z"},
	} {
		if err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("refused arguments reached the server: %+v", *calls)
	}
}

// Without --dry-run the backfill refuses before it contacts the server.
func TestDecisionsBackfillRequiresDryRun(t *testing.T) {
	h, calls := decisionCLIServer(t, `{}`)
	for _, args := range [][]string{
		{"decisions", "backfill"},
		{"decisions", "backfill", "--json"},
		{"decisions", "backfill", "--dry-run=false"},
	} {
		err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--dry-run") {
			t.Errorf("%v: err=%v", args, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("refused backfill called the server: %+v", *calls)
	}
}

func TestDecisionsBackfillTextReport(t *testing.T) {
	h, calls := decisionCLIServer(t, `{"server_time":"2026-11-05T12:00:00Z","items":[
		{"source":"lane_event","source_ref":"12","lane":"ask-lane","created_at":"2026-11-01T12:00:00Z"},
		{"source":"task_flag","source_ref":"5/9","lane":"task-lane","created_at":"2026-11-05T09:00:00Z","task_id":5,"task_state":"developing"}],"truncated":["chat"]}`)
	var out bytes.Buffer
	if err := run([]string{"decisions", "backfill", "--dry-run", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[0], "SOURCE") {
		t.Fatalf("report:\n%s", text)
	}
	// Rows in source order: task_flag before lane_event.
	if f := strings.Fields(lines[1]); len(f) != 9 || f[0] != "task_flag" || f[3] != "3h" || f[4] != "#5" || f[5] != "developing" || f[8] != "live" {
		t.Fatalf("task row %q", lines[1])
	}
	if f := strings.Fields(lines[2]); len(f) != 9 || f[0] != "lane_event" || f[3] != "4d0h" || f[4] != "-" || f[8] != "orphan" {
		t.Fatalf("lane row %q", lines[2])
	}
	for _, want := range []string{
		"by class: live=1 superseded_candidate=0 stale_candidate=0 orphan=1",
		"by source: task_flag=1 task_request=0 job_escalate=0 lane_event=1 chat=0 linear=0",
		"total: 2 (dry run",
		"truncated: chat",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	if c := (*calls)[0]; c.Method != "GET" || c.Path != "/v1/decision-requests/sources" {
		t.Fatalf("call=%+v", c)
	}
}

// sourceTables are every table a decision source or the dry run reads.
var sourceTables = []string{"tasks", "task_events", "relay_events", "chat_questions", "jobs", "linear_decision_issues", "decision_requests", "notification_outbox", "linear_decision_ops"}

func tableDigests(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range sourceTables {
		var n int64
		var digest string
		if err := pool.QueryRow(context.Background(), `SELECT count(*),COALESCE(md5(string_agg(x::text,'|' ORDER BY x::text)),'') FROM `+table+` x`).Scan(&n, &digest); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out[table] = fmt.Sprintf("%d:%s", n, digest)
	}
	return out
}

// The dry run against a seeded server classifies one item per class and
// changes no row of any source table; without --dry-run it exits non-zero
// and changes nothing either.
func TestDecisionsBackfillDryRunWritesNothing(t *testing.T) {
	pool, dbURL := testdb.SchemaPool(t, "decision_backfill")
	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(api.Server{Service: api.Service{Store: st}, Tokens: api.Tokens{"desk": "tok"}}.Handler())
	t.Cleanup(h.Close)
	project := "experiment"
	newTask := func(title string) store.Task {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: "backfill-lane", Title: title, Kind: "implement", CreatedBy: "backfill-test", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}

	// live: a flagged task in a started state.
	live := newTask("live flag")
	if _, err := st.ClaimTask(ctx, live.ID, "backfill-lane", "bf-claim-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(ctx, live.ID, "needs_decision", "backfill-lane", "keep going?", nil, ""); err != nil {
		t.Fatal(err)
	}
	// stale_candidate: a backlog task flagged 20 days ago. task_events is
	// append-only, so the old ask is seeded as it would have been written.
	var stale store.Task
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,awaiting_decision) VALUES('backfill-lane','stale flag','implement','backlog','backfill-test',now()-interval '20 days',now()-interval '20 days',true) RETURNING id`).Scan(&stale.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind,field,from_value,to_value) VALUES($1,'backlog','backlog','backfill-test','still wanted?',now()-interval '20 days','awaiting_decision','awaiting_decision','false','true')`, stale.ID); err != nil {
		t.Fatal(err)
	}
	// superseded_candidate: an open request left on a dropped task.
	dropped := newTask("dropped request")
	if _, err := st.ClaimTask(ctx, dropped.ID, "backfill-lane", "bf-claim-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordDecisionRequest(ctx, dropped.ID, "backfill-lane", store.DecisionRequestInput{
		Question: "which?", DefaultAction: "wait",
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{{Key: "A", Label: "one"}, {Key: "B", Label: "two"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(ctx, dropped.ID, "dropped", "backfill-lane", "not needed", nil, ""); err != nil {
		t.Fatal(err)
	}
	// live: an escalation of a running job.
	jobTask := newTask("job task")
	if _, _, err := st.ClaimJob(ctx, store.Job{JobID: "bf-job-1", TaskID: jobTask.ID, Host: "host-a", CreatedBy: "backfill-test"}); err != nil {
		t.Fatal(err)
	}
	spawned := "spawned"
	if _, err := st.UpdateJob(ctx, "bf-job-1", store.JobUpdate{State: &spawned}); err != nil {
		t.Fatal(err)
	}
	esc, _, err := st.AppendRelayEvent(ctx, store.RelayEvent{Kind: "job.escalate", JobID: "bf-job-1", OwnerLane: "backfill-lane", Question: "go on?", ReportPath: "r"})
	if err != nil {
		t.Fatal(err)
	}
	// orphan: a lane ask and a chat question name no task or job.
	ask, _, err := st.AppendRelayEvent(ctx, store.RelayEvent{Kind: "lane.event", OwnerLane: "backfill-lane", EventID: "bf-ask", Text: "[decision-needed] which?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: "Q-20261105-31", Lane: "backfill-lane", Body: "which?"}); err != nil {
		t.Fatal(err)
	}

	before := tableDigests(t, pool)
	var out bytes.Buffer
	if err := run([]string{"decisions", "backfill", "--dry-run", "--json", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report store.DecisionBackfillReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("out=%s", out.String())
	}
	got := map[string]string{}
	for _, row := range report.Rows {
		got[row.Source+" "+row.SourceRef] = row.Class
	}
	refOf := func(id int64) string {
		var ev int64
		if err := pool.QueryRow(ctx, `SELECT id FROM task_events WHERE task_id=$1 AND kind='awaiting_decision' AND to_value='true' ORDER BY id DESC LIMIT 1`, id).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d/%d", id, ev)
	}
	want := map[string]string{
		"task_flag " + refOf(live.ID):                   store.DecisionClassLive,
		"task_flag " + refOf(stale.ID):                  store.DecisionClassStaleCandidate,
		fmt.Sprintf("task_request dr-%d-1", dropped.ID): store.DecisionClassSupersededCandidate,
		fmt.Sprintf("job_escalate %d", esc.ID):          store.DecisionClassLive,
		fmt.Sprintf("lane_event %d", ask.ID):            store.DecisionClassOrphan,
		"chat Q-20261105-31":                            store.DecisionClassOrphan,
	}
	if len(got) != len(want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
	for ref, class := range want {
		if got[ref] != class {
			t.Errorf("%s: %q, want %s", ref, got[ref], class)
		}
	}
	wantClass := map[string]int{"live": 2, "superseded_candidate": 1, "stale_candidate": 1, "orphan": 2}
	wantSource := map[string]int{"task_flag": 2, "task_request": 1, "job_escalate": 1, "lane_event": 1, "chat": 1, "linear": 0}
	for k, v := range wantClass {
		if report.ByClass[k] != v {
			t.Errorf("by_class[%s]=%d, want %d", k, report.ByClass[k], v)
		}
	}
	for k, v := range wantSource {
		if report.BySource[k] != v {
			t.Errorf("by_source[%s]=%d, want %d", k, report.BySource[k], v)
		}
	}
	if report.Total != 6 || !report.DryRun {
		t.Fatalf("total=%d dry_run=%v", report.Total, report.DryRun)
	}
	if after := tableDigests(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("dry run changed rows:\nbefore %v\nafter  %v", before, after)
	}

	if err := run([]string{"decisions", "backfill", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err == nil {
		t.Fatal("backfill without --dry-run succeeded")
	}
	if after := tableDigests(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("refused backfill changed rows:\nbefore %v\nafter  %v", before, after)
	}
}

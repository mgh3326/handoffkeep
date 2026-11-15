package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// `youtrack links plan` (hk 4731) end to end: the command opens the database
// read-only and reads the fake YouTrack's link front end, so the test sees
// every request the walk made and every row it could have written.

func TestYTLinksPlanRefusesBadArguments(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "")
	for _, args := range [][]string{
		{"youtrack", "links"},
		{"youtrack", "links", "adopt"},
		{"youtrack", "links", "plan", "extra"},
		{"youtrack", "links", "plan", "--task=-1"},
		{"youtrack", "links", "plan", "--limit=-1"},
		{"youtrack", "links", "plan", "--bogus"},
	} {
		if err := run(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "links") {
			t.Errorf("%v: err=%v, want the usage", args, err)
		}
	}
	var exit exitCodeError
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "bogus")
	err := run([]string{"yt", "links", "plan"}, io.Discard, io.Discard)
	if !errors.As(err, &exit) || exit.code != youtrackExitConfig {
		t.Fatalf("unknown mode: %v", err)
	}
	// Missing credentials refuse before any database is opened.
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "off")
	err = run([]string{"yt", "links", "plan"}, io.Discard, io.Discard)
	if !errors.As(err, &exit) || exit.code != youtrackExitConfig || !strings.Contains(err.Error(), "HANDOFFKEEP_DB_URL") {
		t.Fatalf("no config: %v", err)
	}
}

// ytlPlanCLIEnv is one schema bound to one fake YouTrack, with two bound tasks
// related by a live edge (the lower id writes it).
type ytlPlanCLIEnv struct {
	dbURL  string
	tenant string
	fake   *ytwritetest.FakeYouTrack
	lo, hi store.Task
}

func newYTLPlanCLIEnv(t *testing.T, prefix, project string) ytlPlanCLIEnv {
	t.Helper()
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, prefix)
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	fake := ytwritetest.NewFake()
	server := httptest.NewServer(fake.LinkHandler())
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "off")
	t.Setenv("YOUTRACK_URL", server.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "test-token")
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	mk := func(title string) store.Task {
		task, err := st.CreateTask(ctx, store.Task{Lane: "ytl-plan-cli", Title: title, Kind: "implement",
			CreatedBy: "test", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	lo, hi := mk("lower"), mk("higher")
	if lo.ID > hi.ID {
		lo, hi = hi, lo
	}
	for _, task := range []store.Task{lo, hi} {
		id := strconv.FormatInt(task.ID, 10)
		verified, err := st.YTCanaryCandidate(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		b := store.YTTaskBinding{TaskID: task.ID, Tenant: u.Host, IssueID: "2-" + id, IssueReadable: "HK-" + id,
			ProjectID: "0-2", BoundBy: "operator"}
		if _, err := st.AddYTTaskBinding(ctx, b, verified); err != nil {
			t.Fatalf("bind task %d: %v", task.ID, err)
		}
	}
	if _, err := st.AddTaskRelation(ctx, lo.ID, store.RelationRelated, hi.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	return ytlPlanCLIEnv{dbURL: dbURL, tenant: u.Host, fake: fake, lo: lo, hi: hi}
}

// ytlPlanDBCounts is the row count of every table a plan must leave alone.
func ytlPlanDBCounts(t *testing.T, dbURL string) map[string]int64 {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	out := map[string]int64{}
	for _, table := range []string{"yt_write_ops", "yt_link_owned", "yt_link_unowned", "yt_link_plans",
		"yt_link_steps", "yt_links_sweep", "sync_log"} {
		var n int64
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestYTLinksPlanCLIPrintsThePlanAndWritesNothing(t *testing.T) {
	e := newYTLPlanCLIEnv(t, "ytl_plan_cli", "ytl-plan-cli")
	before := ytlPlanDBCounts(t, e.dbURL)

	var out strings.Builder
	if err := run([]string{"youtrack", "links", "plan"}, &out, io.Discard); err != nil {
		t.Fatalf("links plan: %v", err)
	}
	var rep store.YTLinksPlanReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	if rep.Tasks != 2 || len(rep.Rows) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Rows[0].TaskID != e.lo.ID || rep.Rows[0].WouldAdd != 1 || rep.Rows[0].WouldDeleteOwned != 0 {
		t.Fatalf("row = %+v", rep.Rows[0])
	}
	// The fake saw GETs only, and the report counts them.
	gets := 0
	for _, r := range e.fake.LinkRequests() {
		if r.Method != http.MethodGet {
			t.Fatalf("plan sent %s %s", r.Method, r.Path)
		}
		gets++
	}
	if rep.Gets != gets || gets == 0 {
		t.Fatalf("gets = %d, GET requests = %d", rep.Gets, gets)
	}
	if after := ytlPlanDBCounts(t, e.dbURL); !reflect.DeepEqual(after, before) {
		t.Fatalf("plan wrote rows: before %v, after %v", before, after)
	}

	// The output is the documented JSON, field by field.
	golden := fmt.Sprintf(`{"mode":"off","tenant":%q,"tasks":2,"skipped":0,"gets":%d,`+
		`"totals":{"would_add":1,"would_delete_owned":0,"unowned_left":0,"held":0,"already_ok":0},`+
		`"rows":[{"task_id":%d,"issue_id":"2-%d","would_add":1,"would_delete_owned":0,"unowned_left":0,"held":0,"already_ok":0,`+
		`"add":[{"kind":"relates","target_task_id":%d,"target_issue_id":"2-%d"}],"delete":[]}]}`+"\n",
		e.tenant, gets, e.lo.ID, e.lo.ID, e.hi.ID, e.hi.ID)
	if out.String() != golden {
		t.Fatalf("output\n%s\nwant\n%s", out.String(), golden)
	}
}

// --task plans that one bound task, and an unknown task exits not_found.
func TestYTLinksPlanCLITaskFilterAndUnknownTask(t *testing.T) {
	e := newYTLPlanCLIEnv(t, "ytl_plan_cli_task", "ytl-plan-cli-task")
	var out strings.Builder
	if err := run([]string{"youtrack", "links", "plan", "--task", strconv.FormatInt(e.hi.ID, 10)}, &out, io.Discard); err != nil {
		t.Fatalf("links plan --task: %v", err)
	}
	var rep store.YTLinksPlanReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	if rep.Tasks != 1 || rep.TaskID != e.hi.ID || len(rep.Rows) != 0 {
		t.Fatalf("--task %d report = %+v", e.hi.ID, rep)
	}

	err := run([]string{"youtrack", "links", "plan", "--task", "987654321"}, io.Discard, io.Discard)
	var exit exitCodeError
	if !errors.As(err, &exit) || exit.code != youtrackExitNotFound {
		t.Fatalf("unknown task: err=%v, want exit %d", err, youtrackExitNotFound)
	}
}

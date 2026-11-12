package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

const ytdCmdProject = "0-12"

// ytdIssues is a fake issue reader: issues by readable id, and errors.
type ytdIssues struct {
	issues map[string]youtrack.Issue
	errs   map[string]error
	gets   int
}

func (f *ytdIssues) GetIssue(_ context.Context, id string) (youtrack.Issue, error) {
	f.gets++
	if err := f.errs[id]; err != nil {
		return youtrack.Issue{}, err
	}
	issue, ok := f.issues[id]
	if !ok {
		return youtrack.Issue{}, fmt.Errorf("%w: issue %s", youtrack.ErrNotFound, id)
	}
	return issue, nil
}

func ytdIssue(internal string, id int64, summary string, decisionField int64, project string) youtrack.Issue {
	raw, _ := json.Marshal(decisionField)
	return youtrack.Issue{ID: internal, IDReadable: youtrack.DecisionReadable(id), Summary: summary, Project: &youtrack.ProjectRef{ID: project},
		CustomFields: []youtrack.IssueField{{Name: ytwrite.FieldDecisionID, Value: raw}}}
}

// The backfill binds the verified DEC-<id> issue and refuses a mismatched
// one, a missing one and one it could not read; the preflight binds nothing.
func TestYTDecisionsBackfillBindsOnlyVerified(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "ytd_cmd")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids := map[string]int64{}
	for _, qid := range []string{"verified", "summary", "field", "project", "missing", "error"} {
		lane := "ytd-cmd-" + qid
		if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: "Q-YTD-CMD-" + qid, Lane: lane, Body: qid + "?"}); err != nil {
			t.Fatal(err)
		}
		rows, err := st.ListDecisionRecords(ctx, store.DecisionRecordFilter{Source: store.DecisionSourceChat, Lane: lane, Limit: 10}, time.Now().UTC())
		if err != nil || len(rows) != 1 {
			t.Fatalf("decision of %s: %+v %v", qid, rows, err)
		}
		ids[qid] = rows[0].ID
	}
	sum := func(name string) string { return "D-" + strconv.FormatInt(ids[name], 10) + " " + name + "?" }
	fake := &ytdIssues{issues: map[string]youtrack.Issue{
		youtrack.DecisionReadable(ids["verified"]): ytdIssue("2-1", ids["verified"], sum("verified"), ids["verified"], ytdCmdProject),
		youtrack.DecisionReadable(ids["summary"]):  ytdIssue("2-2", ids["summary"], "[결정] other", ids["summary"], ytdCmdProject),
		youtrack.DecisionReadable(ids["field"]):    ytdIssue("2-3", ids["field"], sum("field"), ids["field"]+1000, ytdCmdProject),
		youtrack.DecisionReadable(ids["project"]):  ytdIssue("2-4", ids["project"], sum("project"), ids["project"], "0-99"),
	}, errs: map[string]error{youtrack.DecisionReadable(ids["error"]): errors.New("connection reset")}}
	opts := ytDecisionsOptions{Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test", Snapshots: "off"}

	pre, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Checked != 6 || pre.Verified.Count != 1 || pre.Bound.Count != 0 || pre.Mismatch.Count != 3 || pre.Missing.Count != 1 || pre.Error.Count != 1 {
		t.Fatalf("preflight %+v", pre)
	}
	if _, found, _ := st.YTDecisionBinding(ctx, ids["verified"]); found {
		t.Fatal("the preflight bound a decision")
	}

	opts.Apply = true
	got, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bound.Count != 1 || got.Bound.IDs[0] != ids["verified"] || got.Mismatch.Count != 3 || len(got.Reasons) != 3 {
		t.Fatalf("apply %+v", got)
	}
	b, found, err := st.YTDecisionBinding(ctx, ids["verified"])
	if err != nil || !found || b.IssueID != "2-1" || b.Origin != "import_verified" || b.IssueReadable != youtrack.DecisionReadable(ids["verified"]) || b.ProjectID != ytdCmdProject {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	for _, name := range []string{"summary", "field", "project", "missing", "error"} {
		if _, found, _ := st.YTDecisionBinding(ctx, ids[name]); found {
			t.Fatalf("%s was bound", name)
		}
	}
	// A second run checks only the unbound ones again.
	again, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil || again.Checked != 5 || again.Bound.Count != 0 {
		t.Fatalf("second run %+v %v", again, err)
	}
}

// The decisions mode alone starts the write worker, needs the YouTrack
// credentials, reads the DEC project's id once at start (a failed read stops
// the start), and blocks the legacy push, which also mirrors DEC issues.
func TestYTDecisionsModeStartsTheWriteWorker(t *testing.T) {
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_DECISIONS_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_DECISIONS_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("on without credentials: %v", err)
	}
	if owner, err := youtrackProtocolOwner(); err != nil || owner != "HANDOFFKEEP_YT_DECISIONS_MODE=on" {
		t.Fatalf("protocol owner %q, %v", owner, err)
	}
	projects := http.StatusOK
	reads := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/admin/projects" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		reads++
		if projects != http.StatusOK {
			http.Error(w, "down", projects)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"0-1","shortName":"HK","name":"handoffkeep"},{"id":"0-12","shortName":"DEC","name":"Decisions"}]`))
	}))
	defer ts.Close()
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", readthroughTestKey)
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil || reads != 1 {
		t.Fatalf("decisions on: worker %v err %v reads %d", w, err, reads)
	}
	projects = http.StatusForbidden
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || w != nil || !strings.Contains(err.Error(), "DEC") {
		t.Fatalf("unreadable DEC project: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_DECISIONS_MODE", "yes")
	if _, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_DECISIONS_MODE") {
		t.Fatalf("an unknown decisions mode: %v", err)
	}
	if _, err := youtrackProtocolOwner(); err == nil {
		t.Fatal("an unknown decisions mode let the legacy push run")
	}
	t.Setenv("HANDOFFKEEP_YT_DECISIONS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w != nil {
		t.Fatalf("all off: worker %v err %v", w, err)
	}
}

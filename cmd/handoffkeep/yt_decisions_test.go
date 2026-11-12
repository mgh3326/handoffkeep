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
	for i, qid := range []string{"verified", "summary", "field", "project", "missing", "error", "legacy", "both", "legacybad"} {
		lane := "ytd-cmd-" + qid
		if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: fmt.Sprintf("Q-20261112-%02d", 10+i), Lane: lane, Body: qid + "?"}); err != nil {
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
		// The legacy sync's issue: its own number and summary form, found
		// through youtrack_decision_mirrors.
		"2-9": func() youtrack.Issue {
			i := ytdIssue("2-9", ids["legacy"], "[결정 "+strconv.FormatInt(ids["legacy"], 10)+"] legacy?", ids["legacy"], ytdCmdProject)
			i.IDReadable = "DEC-9001"
			return i
		}(),
		// Both the import's issue and a legacy one verify: ambiguous.
		youtrack.DecisionReadable(ids["both"]): ytdIssue("2-10", ids["both"], sum("both"), ids["both"], ytdCmdProject),
		"2-11":                                 ytdIssue("2-11", ids["both"], "[결정 "+strconv.FormatInt(ids["both"], 10)+"] both?", ids["both"], ytdCmdProject),
		// No DEC-<id>, and the legacy issue's 결정 ID was changed.
		"2-12": ytdIssue("2-12", ids["legacybad"], "[결정 "+strconv.FormatInt(ids["legacybad"], 10)+"] bad?", ids["legacybad"]+1, ytdCmdProject),
	}, errs: map[string]error{youtrack.DecisionReadable(ids["error"]): errors.New("connection reset")}}
	for name, issue := range map[string]string{"legacy": "2-9", "both": "2-11", "legacybad": "2-12"} {
		if err := st.PutYouTrackDecisionMirror(ctx, store.YouTrackDecisionMirror{DecisionID: ids[name], IssueID: issue, DisplayRef: "DEC-x", Stamp: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	opts := ytDecisionsOptions{Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test", Snapshots: "off"}

	pre, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Checked != 9 || pre.Verified.Count != 2 || pre.Legacy.Count != 1 || pre.Ambiguous.Count != 1 || pre.Ambiguous.IDs[0] != ids["both"] ||
		pre.Bound.Count != 0 || pre.Mismatch.Count != 4 || pre.Missing.Count != 1 || pre.Error.Count != 1 {
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
	if got.Bound.Count != 2 || got.Bound.IDs[0] != ids["verified"] || got.Bound.IDs[1] != ids["legacy"] || got.Mismatch.Count != 4 || len(got.Reasons) != 4 {
		t.Fatalf("apply %+v", got)
	}
	if b, found, err := st.YTDecisionBinding(ctx, ids["legacy"]); err != nil || !found || b.IssueID != "2-9" || b.IssueReadable != "DEC-9001" {
		t.Fatalf("legacy binding %+v %v %v", b, found, err)
	}
	b, found, err := st.YTDecisionBinding(ctx, ids["verified"])
	if err != nil || !found || b.IssueID != "2-1" || b.Origin != "import_verified" || b.IssueReadable != youtrack.DecisionReadable(ids["verified"]) || b.ProjectID != ytdCmdProject {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	legacyBad := false
	for _, m := range got.Reasons {
		legacyBad = legacyBad || (m.ID == ids["legacybad"] && strings.Contains(m.Reason, "legacy mirror issue 2-12"))
	}
	if !legacyBad {
		t.Fatalf("legacy mismatch reason lost: %+v", got.Reasons)
	}
	for _, name := range []string{"summary", "field", "project", "missing", "error", "both", "legacybad"} {
		if _, found, _ := st.YTDecisionBinding(ctx, ids[name]); found {
			t.Fatalf("%s was bound", name)
		}
	}
	// A second run checks only the unbound ones again.
	again, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil || again.Checked != 7 || again.Bound.Count != 0 {
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

type fakeYTDStore struct {
	unbound  []store.YTDecisionCandidate
	bound    []int64
	mirrors  map[int64]store.YouTrackDecisionMirror
	bindings map[int64]store.YTDecisionBinding
	outcomes map[int64]store.YTDecisionOutcome
	calls    map[int64]int
}

func (f *fakeYTDStore) UnboundYTDecisions(_ context.Context, afterID int64, limit int) ([]store.YTDecisionCandidate, error) {
	out := []store.YTDecisionCandidate{}
	for _, c := range f.unbound {
		if c.ID > afterID {
			out = append(out, c)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeYTDStore) BoundYTDecisions(_ context.Context, _ string, afterID int64, limit int) ([]int64, error) {
	out := []int64{}
	for _, id := range f.bound {
		if id > afterID {
			out = append(out, id)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeYTDStore) BindYTDecision(_ context.Context, b store.YTDecisionBinding, _ time.Time) (bool, error) {
	if f.bindings == nil {
		f.bindings = map[int64]store.YTDecisionBinding{}
	}
	f.bindings[b.DecisionID] = b
	f.bound = append(f.bound, b.DecisionID)
	return true, nil
}

func (f *fakeYTDStore) PrepareYTDecision(_ context.Context, id int64, _ time.Time) (store.YTDecisionOutcome, error) {
	if f.calls == nil {
		f.calls = map[int64]int{}
	}
	f.calls[id]++
	return f.outcomes[id], nil
}

func (f *fakeYTDStore) ListYouTrackDecisionMirrors(_ context.Context, _ []int64) (map[int64]store.YouTrackDecisionMirror, error) {
	return f.mirrors, nil
}

// Unit test for the case in the comment: 6 decisions bound while mode was off,
// rerun with mode on prepares snapshots for the 6 bound decisions and
// decision_create for missing decision 7; second rerun is idempotent.
func TestYTDecisionsBackfillRunUnit_CommentCase(t *testing.T) {
	ctx := context.Background()
	fakeIssues := &ytdIssues{issues: map[string]youtrack.Issue{}, errs: map[string]error{}}
	st := &fakeYTDStore{
		unbound: []store.YTDecisionCandidate{{ID: 7}},
		bound:   []int64{1, 2, 3, 4, 5, 6},
		outcomes: map[int64]store.YTDecisionOutcome{
			1: store.YTDecisionFieldsPrepared,
			2: store.YTDecisionFieldsPrepared,
			3: store.YTDecisionFieldsPrepared,
			4: store.YTDecisionFieldsPrepared,
			5: store.YTDecisionFieldsPrepared,
			6: store.YTDecisionFieldsPrepared,
			7: store.YTDecisionCreatePrepared,
		},
	}
	opts := ytDecisionsOptions{Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test",
		Apply: true, Snapshots: string(store.YTDecisionsOn)}

	r, err := ytDecisionsBackfillRun(ctx, st, fakeIssues, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Checked != 7 || r.Missing.Count != 1 || r.Missing.IDs[0] != 7 ||
		r.CreatePrepared.Count != 1 || r.CreatePrepared.IDs[0] != 7 ||
		r.FieldsPrepared.Count != 6 || len(r.FieldsPrepared.IDs) != 6 {
		t.Fatalf("run with mode on: %+v", r)
	}

	// Idempotency check: rerun with mode on.
	st.unbound = []store.YTDecisionCandidate{{ID: 7, PendingOp: true}}
	for i := int64(1); i <= 6; i++ {
		st.outcomes[i] = store.YTDecisionUnchanged
	}
	again, err := ytDecisionsBackfillRun(ctx, st, fakeIssues, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Checked != 7 || again.PendingOp.Count != 1 || again.CreatePrepared.Count != 0 ||
		again.FieldsPrepared.Count != 0 || again.Unchanged.Count != 6 {
		t.Fatalf("idempotent rerun: %+v", again)
	}
}

// Unit test negative cases: mode off and preflight (apply=false).
func TestYTDecisionsBackfillRunUnit_NegativeCases(t *testing.T) {
	ctx := context.Background()
	fakeIssues := &ytdIssues{issues: map[string]youtrack.Issue{}, errs: map[string]error{}}
	st := &fakeYTDStore{
		unbound: []store.YTDecisionCandidate{{ID: 7}},
		bound:   []int64{1, 2, 3},
		outcomes: map[int64]store.YTDecisionOutcome{
			1: store.YTDecisionFieldsPrepared,
			2: store.YTDecisionFieldsPrepared,
			3: store.YTDecisionFieldsPrepared,
			7: store.YTDecisionCreatePrepared,
		},
	}

	// Negative case 1: Apply with mode off does not prepare create or snapshots.
	optsOff := ytDecisionsOptions{Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test",
		Apply: true, Snapshots: string(store.YTDecisionsOff)}
	rOff, err := ytDecisionsBackfillRun(ctx, st, fakeIssues, optsOff)
	if err != nil {
		t.Fatal(err)
	}
	if rOff.Checked != 1 || rOff.Missing.Count != 1 || rOff.CreatePrepared.Count != 0 || rOff.FieldsPrepared.Count != 0 {
		t.Fatalf("mode off: %+v", rOff)
	}
	if len(st.calls) != 0 {
		t.Fatalf("prepare calls made with mode off: %+v", st.calls)
	}

	// Negative case 2: Preflight (Apply=false) with mode on writes nothing.
	optsPre := ytDecisionsOptions{Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test",
		Apply: false, Snapshots: string(store.YTDecisionsOn)}
	rPre, err := ytDecisionsBackfillRun(ctx, st, fakeIssues, optsPre)
	if err != nil {
		t.Fatal(err)
	}
	if rPre.Checked != 1 || rPre.Missing.Count != 1 || rPre.CreatePrepared.Count != 0 || rPre.FieldsPrepared.Count != 0 {
		t.Fatalf("preflight: %+v", rPre)
	}
	if len(st.calls) != 0 {
		t.Fatalf("prepare calls made in preflight: %+v", st.calls)
	}
}

// DB test for the case in the comment: 6 decisions bound without snapshot while
// mode was off, rerun with mode on prepares decision_fields for the 6 bound
// decisions and decision_create for missing decision 7; second rerun is idempotent.
func TestYTDecisionsBackfillPreparesSnapshotsAndMissingCreateInModeOn(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "ytd_snap")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ids := make([]int64, 7)
	for i := 0; i < 7; i++ {
		lane := fmt.Sprintf("ytd-snap-%d", i+1)
		qid := fmt.Sprintf("Q-20261112-%02d", 30+i)
		if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: qid, Lane: lane, Body: fmt.Sprintf("q%d?", i+1)}); err != nil {
			t.Fatal(err)
		}
		rows, err := st.ListDecisionRecords(ctx, store.DecisionRecordFilter{Source: store.DecisionSourceChat, Lane: lane, Limit: 10}, time.Now().UTC())
		if err != nil || len(rows) != 1 {
			t.Fatalf("decision %d: %+v %v", i+1, rows, err)
		}
		ids[i] = rows[0].ID
	}

	// Decisions 1..6: bind directly with mode off (no snapshot prepared).
	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		if _, err := st.BindYTDecision(ctx, store.YTDecisionBinding{
			DecisionID: ids[i], Tenant: "tenant.youtrack.cloud", IssueID: fmt.Sprintf("2-%d", 100+i),
			IssueReadable: youtrack.DecisionReadable(ids[i]), ProjectID: ytdCmdProject, BoundBy: "operator:test",
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	// Decision 7 is missing in YouTrack (not in issues map).
	fake := &ytdIssues{issues: map[string]youtrack.Issue{}}
	st.SetYTDecisionsMode(store.YTDecisionsOn, "tenant.youtrack.cloud", ytdCmdProject, "DEC")
	opts := ytDecisionsOptions{
		Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test",
		Apply: true, Snapshots: string(store.YTDecisionsOn),
	}

	r, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Checked != 7 || r.Missing.Count != 1 || r.Missing.IDs[0] != ids[6] ||
		r.CreatePrepared.Count != 1 || r.CreatePrepared.IDs[0] != ids[6] ||
		r.FieldsPrepared.Count != 6 {
		t.Fatalf("first run with mode on: %+v", r)
	}

	// Second run: idempotent, nothing new prepared.
	again, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Checked != 7 || again.PendingOp.Count != 1 || again.CreatePrepared.Count != 0 ||
		again.FieldsPrepared.Count != 0 || again.Unchanged.Count != 6 {
		t.Fatalf("second run with mode on: %+v", again)
	}
}

// DB test negative case: mode off does not prepare snapshots for bound decisions
// and does not prepare decision_create for missing decisions.
func TestYTDecisionsBackfillModeOffNegativeCase(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "ytd_neg")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ids := make([]int64, 3)
	for i := 0; i < 3; i++ {
		lane := fmt.Sprintf("ytd-neg-%d", i+1)
		qid := fmt.Sprintf("Q-20261112-%02d", 40+i)
		if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: qid, Lane: lane, Body: fmt.Sprintf("qneg%d?", i+1)}); err != nil {
			t.Fatal(err)
		}
		rows, err := st.ListDecisionRecords(ctx, store.DecisionRecordFilter{Source: store.DecisionSourceChat, Lane: lane, Limit: 10}, time.Now().UTC())
		if err != nil || len(rows) != 1 {
			t.Fatalf("decision %d: %+v %v", i+1, rows, err)
		}
		ids[i] = rows[0].ID
	}

	// Bind decisions 1 and 2 directly.
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if _, err := st.BindYTDecision(ctx, store.YTDecisionBinding{
			DecisionID: ids[i], Tenant: "tenant.youtrack.cloud", IssueID: fmt.Sprintf("2-%d", 200+i),
			IssueReadable: youtrack.DecisionReadable(ids[i]), ProjectID: ytdCmdProject, BoundBy: "operator:test",
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	fake := &ytdIssues{issues: map[string]youtrack.Issue{}}
	opts := ytDecisionsOptions{
		Tenant: "tenant.youtrack.cloud", ProjectID: ytdCmdProject, BindBy: "operator:test",
		Apply: true, Snapshots: string(store.YTDecisionsOff),
	}

	r, err := ytDecisionsBackfillRun(ctx, st, fake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Checked != 1 || r.Missing.Count != 1 || r.CreatePrepared.Count != 0 || r.FieldsPrepared.Count != 0 {
		t.Fatalf("mode off: %+v", r)
	}
}

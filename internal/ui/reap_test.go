package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// testdata/panewire-session-reap.json is the GET /v1/session-reap body the
// panewire hub (#603, hub_session_reap.go) served for its own fixture report,
// captured from the hub handler rather than written by hand. Its node judged
// 20 live panes: one worker candidate, two builder-task-gate rows, eleven
// held rows, six human sessions counted only.

var reapNow = time.Date(2026, 9, 23, 13, 0, 0, 0, time.UTC)

func reapFixtureNodes(t *testing.T) []hubReapNode {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "panewire-session-reap.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapped struct {
		Nodes []hubReapNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatal(err)
	}
	if len(wrapped.Nodes) != 1 || len(wrapped.Nodes[0].Report.Rows) != 14 {
		t.Fatalf("fixture drifted: %+v", wrapped.Nodes)
	}
	return wrapped.Nodes
}

func reapTask(id int64, jobID, state string, updated time.Time) store.Task {
	return store.Task{ID: id, State: state, Refs: store.TaskRefs{JobID: jobID}, UpdatedAt: updated}
}

func reapByPane(rows []reapRow) map[string]reapRow {
	out := make(map[string]reapRow, len(rows))
	for _, row := range rows {
		out[row.PaneID] = row
	}
	return out
}

const (
	reapMergedBuilderJob = "599-merged-builder-20260923"
	reapOpenBuilderJob   = "529-deploy-view-20260923-1535"
)

func TestProjectReapFixture(t *testing.T) {
	nodes := reapFixtureNodes(t)
	tasks := []store.Task{
		reapTask(599, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour)),
		reapTask(529, reapOpenBuilderJob, "in_progress", reapNow.Add(-time.Hour)),
		// Prefix trap: never joins job "601-verify-20260923-1100".
		reapTask(601, "601-verify", "merged", reapNow.Add(-time.Hour)),
	}
	candidates, held, nodeRows := projectReap(nodes, tasks, false, reapNow)
	byPane := reapByPane(candidates)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v", candidates)
	}
	worker := byPane["w2:p25"]
	if worker.Basis != "job-terminal" || worker.JobID != "601-verify-20260923-1100" || worker.TaskID != nil || worker.AgentName != "t601-verify" {
		t.Fatalf("worker candidate = %+v", worker)
	}
	builder := byPane["w2:p32"]
	if builder.Basis != "task-terminal" || builder.TaskID == nil || *builder.TaskID != 599 || builder.TaskState != "merged" {
		t.Fatalf("builder candidate = %+v", builder)
	}
	heldByPane := reapByPane(held)
	if row := heldByPane["w2:p3"]; row.Reason != "task-open" || row.TaskID == nil || *row.TaskID != 529 {
		t.Fatalf("open-task builder = %+v", row)
	}
	if len(held) != 12 {
		t.Fatalf("held = %d rows: %+v", len(held), held)
	}
	for _, row := range held {
		if row.Reason == "" {
			t.Errorf("held row without reason: %+v", row)
		}
	}
	if len(nodeRows) != 1 || nodeRows[0].Summary.NoJob != 6 || nodeRows[0].GraceSeconds != 600 {
		t.Fatalf("node rows = %+v", nodeRows)
	}
}

// The builder rule: a builder is a candidate only after its linked task is
// merged or dropped and has stayed so past the grace.
func TestProjectReapBuilderTaskGate(t *testing.T) {
	for _, test := range []struct {
		name      string
		tasks     []store.Task
		truncated bool
		reason    string
	}{
		{"no linked task", nil, false, "task-unlinked"},
		{"prefix is not a link", []store.Task{reapTask(1, "599-merged-builder", "merged", reapNow.Add(-time.Hour))}, false, "task-unlinked"},
		{"task still open", []store.Task{reapTask(1, reapMergedBuilderJob, "verifying", reapNow.Add(-time.Hour))}, false, "task-open"},
		{"task on hold", []store.Task{reapTask(1, reapMergedBuilderJob, "join", reapNow.Add(-time.Hour))}, false, "task-open"},
		{"merged one second short of grace", []store.Task{reapTask(1, reapMergedBuilderJob, "merged", reapNow.Add(-10*time.Minute+time.Second))}, false, "task-within-grace"},
		{"merged exactly at grace", []store.Task{reapTask(1, reapMergedBuilderJob, "merged", reapNow.Add(-10*time.Minute))}, false, ""},
		{"dropped past grace", []store.Task{reapTask(1, reapMergedBuilderJob, "dropped", reapNow.Add(-time.Hour))}, false, ""},
		{"no updated_at", []store.Task{reapTask(1, reapMergedBuilderJob, "merged", time.Time{})}, false, "task-within-grace"},
		{"two linked tasks", []store.Task{reapTask(1, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour)), reapTask(2, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour))}, false, "task-ambiguous"},
		{"task list truncated", []store.Task{reapTask(1, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour))}, true, "tasks-truncated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidates, held, _ := projectReap(reapFixtureNodes(t), test.tasks, test.truncated, reapNow)
			candidate, isCandidate := reapByPane(candidates)["w2:p32"]
			if test.reason == "" {
				if !isCandidate || candidate.Basis != "task-terminal" {
					t.Fatalf("want candidate, held=%+v", reapByPane(held)["w2:p32"])
				}
				return
			}
			if isCandidate {
				t.Fatalf("builder became a candidate: %+v", candidate)
			}
			if row := reapByPane(held)["w2:p32"]; row.Reason != test.reason {
				t.Fatalf("held reason = %q, want %q", row.Reason, test.reason)
			}
		})
	}
}

// The console never raises a node verdict: held rows keep their reason, and
// a report from a node that is not connected yields no candidates at all.
func TestProjectReapOnlyLowers(t *testing.T) {
	merged := []store.Task{reapTask(599, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour))}
	nodes := reapFixtureNodes(t)
	_, held, _ := projectReap(nodes, merged, false, reapNow)
	for pane, reason := range map[string]string{"w2:p26": "protected", "w2:p27": "label-mismatch", "w2:p28": "label-unknown", "w6:p1": "label-unknown", "w2:p29": "protected-role"} {
		if row := reapByPane(held)[pane]; row.Reason != reason {
			t.Errorf("%s reason = %q, want node reason %q", pane, row.Reason, reason)
		}
	}
	for _, state := range []string{"stale", "disconnected"} {
		stale := reapFixtureNodes(t)
		stale[0].State, stale[0].Stale = state, true
		candidates, held, _ := projectReap(stale, merged, false, reapNow)
		if len(candidates) != 0 {
			t.Fatalf("%s node produced candidates: %+v", state, candidates)
		}
		if row := reapByPane(held)["w2:p25"]; row.Reason != "node-not-connected" {
			t.Fatalf("%s worker row = %+v", state, row)
		}
	}
	// A candidate whose shape the hub would reject is held, not trusted.
	odd := reapFixtureNodes(t)
	odd[0].Report.Rows[0].Role = "builder"
	if candidates, _, _ := projectReap(odd, nil, false, reapNow); len(reapByPane(candidates)) != 0 {
		t.Fatalf("builder-role candidate row was trusted: %+v", candidates)
	}
	unknown := reapFixtureNodes(t)
	unknown[0].Report.Rows[0].Class = "close-now"
	if candidates, held, _ := projectReap(unknown, nil, false, reapNow); len(candidates) != 0 || reapByPane(held)["w2:p25"].Reason != "unknown-class" {
		t.Fatalf("unknown class: candidates=%+v", candidates)
	}
}

// A report whose schema this join does not know, or whose grace is not
// positive, cannot vouch for anything: no candidates, reason report-shape.
func TestProjectReapReportShape(t *testing.T) {
	merged := []store.Task{reapTask(599, reapMergedBuilderJob, "merged", reapNow.Add(-time.Hour))}
	future := reapFixtureNodes(t)
	future[0].Report.Schema = 2
	candidates, held, _ := projectReap(future, merged, false, reapNow)
	if len(candidates) != 0 || reapByPane(held)["w2:p25"].Reason != "report-shape" || reapByPane(held)["w2:p32"].Reason != "report-shape" {
		t.Fatalf("schema 2: candidates=%+v", candidates)
	}
	for _, grace := range []int64{0, -600} {
		nodes := reapFixtureNodes(t)
		nodes[0].Report.GraceSeconds = grace
		candidates, held, _ := projectReap(nodes, merged, false, reapNow)
		if _, promoted := reapByPane(candidates)["w2:p32"]; promoted || reapByPane(held)["w2:p32"].Reason != "report-shape" {
			t.Fatalf("grace %d promoted the builder: %+v", grace, candidates)
		}
	}
}

package plane

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func reconcileTasksSource(st *store.Store) func(context.Context) ([]store.Task, error) {
	return func(ctx context.Context) ([]store.Task, error) {
		tasks := []store.Task{}
		var afterID int64
		for {
			page, err := st.ListTasksPage(ctx, "", "", "", nil, afterID, 500)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, page...)
			if len(page) < 500 {
				return tasks, nil
			}
			afterID = page[len(page)-1].ID
		}
	}
}

// TestReconcileEnqueuesCorrectiveUpdate proves hk wins: a remote item whose
// name drifted produces a drift record and exactly one corrective outbox op
// carrying the full projected snapshot.
func TestReconcileEnqueuesCorrectiveUpdate(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-drift", "proj-exp")
	fake.seed(Issue{
		ID:              "wi-drift",
		Name:            "renamed on the Plane board",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Priority:        "medium",
		Labels:          []string{"lb-lane", "lb-kind"},
		Project:         "proj-exp",
	})
	reconciler := &Reconciler{
		Client:        fake.client(t),
		Projects:      testMapping(),
		ListTasks:     reconcileTasksSource(st),
		LinkedIssues:  st.ListPlaneIssues,
		EnqueueUpdate: st.EnqueuePlaneUpdate,
	}
	report, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Enqueued != 1 {
		t.Fatalf("enqueued=%d drifts=%+v", report.Enqueued, report.Drifts)
	}
	nameDrift := false
	for _, drift := range report.Drifts {
		if drift.Field == "name" && drift.Expected == "Mirror connector contract" {
			nameDrift = true
		}
	}
	if !nameDrift {
		t.Fatalf("no name drift recorded: %+v", report.Drifts)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 2 || rows[1].Op != store.PlaneOpWorkItemUpdate {
		t.Fatalf("corrective rows=%+v err=%v", rows, err)
	}
	if rows[1].Payload.Name != "Mirror connector contract" {
		t.Fatalf("corrective payload=%+v", rows[1].Payload)
	}
}

// TestReconcileDryRunCountsWithoutEnqueueing proves the read-only pass
// reports the same drift but writes no corrective ops and no document.
func TestReconcileDryRunCountsWithoutEnqueueing(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-drift", "proj-exp")
	fake.seed(Issue{
		ID:              "wi-drift",
		Name:            "renamed",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Priority:        "medium",
		Labels:          []string{"lb-lane", "lb-kind"},
		Project:         "proj-exp",
	})
	reconciler := &Reconciler{
		Client:       fake.client(t),
		Projects:     testMapping(),
		ListTasks:    reconcileTasksSource(st),
		LinkedIssues: st.ListPlaneIssues,
		DryRun:       true,
	}
	report, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Enqueued != 0 || len(report.Drifts) == 0 {
		t.Fatalf("dry-run enqueued=%d drifts=%+v", report.Enqueued, report.Drifts)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("dry-run enqueued rows=%+v err=%v", rows, err)
	}
}

// TestReconcileMissingRemoteIsPresenceDrift covers the mirror-drift case the
// exit plan warns about: a live projection link whose remote item vanished.
func TestReconcileMissingRemoteIsPresenceDrift(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-gone", "proj-exp")
	reconciler := &Reconciler{
		Client:        fake.client(t),
		Projects:      testMapping(),
		ListTasks:     reconcileTasksSource(st),
		LinkedIssues:  st.ListPlaneIssues,
		EnqueueUpdate: st.EnqueuePlaneUpdate,
	}
	report, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	presence := false
	for _, drift := range report.Drifts {
		if drift.TaskID == task.ID && drift.Field == "presence" && drift.Expected == "present" && drift.Actual == "missing" {
			presence = true
		}
	}
	if !presence || report.Enqueued != 1 {
		t.Fatalf("presence=%t enqueued=%d drifts=%+v", presence, report.Enqueued, report.Drifts)
	}
}

func TestEvaluateMirrorCounts(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	project := "experiment"
	other := "handoffkeep"
	tasks := []store.Task{
		{ID: 1, State: "backlog", Title: "open a", Project: &project, UpdatedAt: recent},
		{ID: 2, State: "in_progress", Title: "open b", Project: &other, UpdatedAt: recent},
		{ID: 3, State: "merged", Title: "done", Project: &project, UpdatedAt: recent},
		{ID: 4, State: "dropped", Title: "old dropped", Project: &project, UpdatedAt: old},
		{ID: 5, State: "claimed", Title: "sensitive 10.0.0.1 host", Project: &project, UpdatedAt: recent},
		{ID: 6, State: "backlog", Title: "no project", UpdatedAt: old},
	}
	mapping := ProjectMapping{Map: map[string]string{"experiment": "EXP"}, Default: ""}
	plan := EvaluateMirror(tasks, now, mapping, 250)
	if plan.Tasks != 6 || plan.Mirrored != 4 || plan.Terminal != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	if plan.TouchedLast7d != 4 || plan.TerminalLast7d != 1 {
		t.Fatalf("churn=%+v", plan)
	}
	if plan.Unmapped != 2 || plan.UnmappedProjects["handoffkeep"] != 1 || plan.UnmappedProjects["(unset)"] != 1 {
		t.Fatalf("unmapped=%+v", plan)
	}
	if plan.SensitiveTitles != 1 {
		t.Fatalf("sensitive=%d", plan.SensitiveTitles)
	}
	if plan.LinearHeadroom != 246 || !plan.LinearFits {
		t.Fatalf("headroom=%d fits=%t", plan.LinearHeadroom, plan.LinearFits)
	}
	if plan.EstimatedCallsPerDay <= 0 {
		t.Fatalf("calls/day=%f", plan.EstimatedCallsPerDay)
	}
	mapped := ProjectMapping{Map: map[string]string{"experiment": "EXP"}, Default: "HK"}
	if plan := EvaluateMirror(tasks, now, mapped, 250); plan.Unmapped != 0 {
		t.Fatalf("default should cover unmapped: %+v", plan)
	}
}

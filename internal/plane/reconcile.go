package plane

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	// ReconcileInterval is the absorbed requirement's daily snapshot
	// re-projection cadence: once a day every mirrored task is compared
	// against its remote work item.
	ReconcileInterval   = 24 * time.Hour
	ReconcilePassBudget = 5 * time.Minute
)

type Drift struct {
	TaskID     int64  `json:"task_id"`
	WorkItemID string `json:"work_item_id,omitempty"`
	Field      string `json:"field"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
}

type ReconcileReport struct {
	Key      string                  `json:"key"`
	Drifts   []Drift                 `json:"drifts"`
	Enqueued int                     `json:"enqueued"`
	Outbox   store.PlaneOutboxStatus `json:"outbox"`
	Body     string                  `json:"body"`
	Changed  bool                    `json:"changed"`
}

// Reconciler compares the hk projection with the remote board on a daily
// pass. Drift resolves one way — hk wins — by enqueuing a corrective
// work_item_update per drifted task; the drain then overwrites the remote
// fields. The remote is never read back into hk.
type Reconciler struct {
	Client        *Client
	Projects      ProjectMapping
	ListTasks     func(context.Context) ([]store.Task, error)
	LinkedIssues  func(context.Context) ([]store.PlaneIssue, error)
	EnqueueUpdate func(context.Context, int64) error
	OutboxStatus  func(context.Context) (store.PlaneOutboxStatus, error)
	WriteDocument func(context.Context, store.Document) (store.Document, bool, error)
	Interval      time.Duration
	PassBudget    time.Duration
	Now           func() time.Time
	// DryRun reports drift and counts the corrective ops it would enqueue
	// without enqueueing them.
	DryRun bool
	Logger *log.Logger
}

func (reconciler *Reconciler) logf(format string, values ...any) {
	if reconciler.Logger != nil {
		reconciler.Logger.Printf(format, values...)
	}
}

func (reconciler *Reconciler) stateNames(ctx context.Context, projectID string) (map[string]string, error) {
	states, err := reconciler.Client.ListStates(ctx, projectID)
	if err != nil {
		return nil, err
	}
	byID := map[string]string{}
	for _, state := range states {
		byID[state.ID] = state.Name
	}
	return byID, nil
}

func (reconciler *Reconciler) labelNames(ctx context.Context, projectID string) (map[string]string, error) {
	labels, err := reconciler.Client.ListLabels(ctx, projectID)
	if err != nil {
		return nil, err
	}
	byID := map[string]string{}
	for _, label := range labels {
		byID[label.ID] = label.Name
	}
	return byID, nil
}

func formatReconcileBody(date string, drifts []Drift, enqueued int, status store.PlaneOutboxStatus) string {
	lines := []string{
		"# Plane reconcile " + date,
		"",
		fmt.Sprintf("Outbox pending: %d", status.Pending),
		fmt.Sprintf("Outbox failed: %d", status.Failed),
		fmt.Sprintf("Outbox dry-run: %d", status.DryRun),
	}
	if status.LastError != "" {
		lines = append(lines, "Outbox last error: "+status.LastError)
	}
	lines = append(lines,
		"",
		fmt.Sprintf("Drifts before re-projection: %d", len(drifts)),
		fmt.Sprintf("Corrective ops enqueued: %d", enqueued),
		"Drift policy: hk wins; each drift enqueues one work_item_update carrying the full projected snapshot.",
		"",
	)
	for _, drift := range drifts {
		line := fmt.Sprintf("- task %d", drift.TaskID)
		if drift.WorkItemID != "" {
			line += " item " + drift.WorkItemID
		}
		line += ": " + drift.Field
		if drift.Expected != "" || drift.Actual != "" {
			line += fmt.Sprintf(" expected=%q actual=%q", drift.Expected, drift.Actual)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

// compare returns the fields where the remote work item disagrees with the
// hk projection. The comparison is allowlist-shaped — the same fields the
// writer is allowed to send are the only fields drift is measured on. The
// project compare needs id-to-name translation, so the caller does it after
// resolving the remote's project identifier.
func compareProjection(payload store.PlaneOutboxPayload, remote Issue, stateNames, labelNames map[string]string) []Drift {
	drifts := []Drift{}
	if remote.Name != payload.Name {
		drifts = append(drifts, Drift{Field: "name", Expected: payload.Name, Actual: remote.Name})
	}
	if payload.State != "" && stateNames[remote.State] != payload.State {
		drifts = append(drifts, Drift{Field: "state", Expected: payload.State, Actual: stateNames[remote.State]})
	}
	if payload.Priority != "" && remote.Priority != payload.Priority {
		drifts = append(drifts, Drift{Field: "priority", Expected: payload.Priority, Actual: remote.Priority})
	}
	expectedLabels := planeLabelNames(payload)
	sort.Strings(expectedLabels)
	actualLabels := []string{}
	for _, id := range remote.Labels {
		if name := labelNames[id]; name != "" {
			actualLabels = append(actualLabels, name)
		}
	}
	sort.Strings(actualLabels)
	if strings.Join(expectedLabels, ",") != strings.Join(actualLabels, ",") {
		drifts = append(drifts, Drift{Field: "labels", Expected: strings.Join(expectedLabels, ","), Actual: strings.Join(actualLabels, ",")})
	}
	return drifts
}

func (reconciler *Reconciler) RunOnce(ctx context.Context) (ReconcileReport, error) {
	if reconciler.Client == nil || reconciler.ListTasks == nil || (!reconciler.DryRun && reconciler.EnqueueUpdate == nil) {
		return ReconcileReport{}, fmt.Errorf("plane reconciler requires a client, task source, and (unless dry-run) an enqueue hook")
	}
	passBudget := reconciler.PassBudget
	if passBudget <= 0 {
		passBudget = ReconcilePassBudget
	}
	passCtx, cancel := context.WithTimeout(ctx, passBudget)
	defer cancel()
	ctx = passCtx
	now := time.Now().UTC()
	if reconciler.Now != nil {
		now = reconciler.Now().UTC()
	}
	date := now.Format("2006-01-02")
	report := ReconcileReport{Key: "report/plane/reconcile/" + date, Drifts: []Drift{}}
	tasks, err := reconciler.ListTasks(ctx)
	if err != nil {
		return report, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	linkedByTask := map[int64]store.PlaneIssue{}
	if reconciler.LinkedIssues != nil {
		links, err := reconciler.LinkedIssues(ctx)
		if err != nil {
			return report, err
		}
		for _, link := range links {
			linkedByTask[link.TaskID] = link
		}
	}
	projectNames := map[string]string{}
	stateNames := map[string]map[string]string{}
	labelNames := map[string]map[string]string{}
	drifted := map[int64]bool{}
	for _, task := range tasks {
		_, linked := linkedByTask[task.ID]
		if store.PlaneTaskTerminal(task) && !linked {
			continue
		}
		payload, projErr := store.PlaneProjectionFor(task)
		if projErr != nil {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "projection", Actual: projErr.Error()})
			continue
		}
		identifier, mapErr := reconciler.Projects.IdentifierFor(payload.Project)
		if mapErr != nil {
			if linked || !store.PlaneTaskTerminal(task) {
				report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "project_map", Actual: mapErr.Error()})
			}
			continue
		}
		projectID, err := reconciler.Client.ProjectIDFor(ctx, identifier)
		if err != nil {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "project_lookup", Actual: err.Error()})
			continue
		}
		var remote Issue
		found := false
		if linked {
			remote, found, err = reconciler.Client.GetIssue(ctx, linkedByTask[task.ID].ProjectID, linkedByTask[task.ID].WorkItemID)
			if err != nil {
				report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "lookup_error", Actual: err.Error()})
				continue
			}
		}
		if !found {
			remote, found, err = reconciler.Client.FindIssueByMarker(ctx, projectID, markerForTask(task.ID))
			if err != nil {
				report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "lookup_error", Actual: err.Error()})
				continue
			}
		}
		if store.PlaneTaskTerminal(task) {
			// A terminal task with a live remote item is drift: the
			// projection carries non-terminal tasks only.
			if found {
				report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, WorkItemID: remote.ID, Field: "presence", Expected: "absent", Actual: "present"})
				drifted[task.ID] = true
			}
			continue
		}
		if !found {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "presence", Expected: "present", Actual: "missing"})
			drifted[task.ID] = true
			continue
		}
		if _, ok := projectNames[remote.Project]; !ok {
			if name, err := reconciler.Client.ProjectIdentifierFor(ctx, remote.Project); err == nil {
				projectNames[remote.Project] = name
			}
		}
		if _, ok := stateNames[remote.Project]; !ok {
			if names, err := reconciler.stateNames(ctx, remote.Project); err == nil {
				stateNames[remote.Project] = names
			}
		}
		if _, ok := labelNames[remote.Project]; !ok {
			if names, err := reconciler.labelNames(ctx, remote.Project); err == nil {
				labelNames[remote.Project] = names
			}
		}
		itemDrifts := compareProjection(payload, remote, stateNames[remote.Project], labelNames[remote.Project])
		if remote.Project != projectID {
			itemDrifts = append(itemDrifts, Drift{Field: "project", Expected: identifier, Actual: projectNames[remote.Project]})
		}
		for _, drift := range itemDrifts {
			drift.TaskID = task.ID
			drift.WorkItemID = remote.ID
			report.Drifts = append(report.Drifts, drift)
		}
		if len(itemDrifts) > 0 {
			drifted[task.ID] = true
		}
	}
	for taskID := range drifted {
		if reconciler.DryRun {
			continue
		}
		if err := reconciler.EnqueueUpdate(ctx, taskID); err != nil {
			report.Drifts = append(report.Drifts, Drift{TaskID: taskID, Field: "enqueue_error", Actual: err.Error()})
			continue
		}
		report.Enqueued++
	}
	if reconciler.OutboxStatus != nil {
		report.Outbox, err = reconciler.OutboxStatus(ctx)
		if err != nil {
			return report, err
		}
	}
	report.Body = formatReconcileBody(date, report.Drifts, report.Enqueued, report.Outbox)
	if reconciler.DryRun || reconciler.WriteDocument == nil {
		return report, nil
	}
	_, report.Changed, err = reconciler.WriteDocument(ctx, store.Document{Key: report.Key, Kind: "report", Body: report.Body})
	return report, err
}

func (reconciler *Reconciler) Run(ctx context.Context) {
	interval := reconciler.Interval
	if interval <= 0 {
		interval = ReconcileInterval
	}
	run := func() {
		if _, err := reconciler.RunOnce(ctx); err != nil && ctx.Err() == nil {
			reconciler.logf("plane reconcile error: %v", err)
		}
	}
	for {
		run()
		if !waitFor(ctx, interval) {
			return
		}
	}
}

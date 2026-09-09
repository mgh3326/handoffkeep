package linear

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const ReconcileInterval = 30 * time.Minute

type Drift struct {
	TaskID   int64  `json:"task_id"`
	IssueID  string `json:"issue_id,omitempty"`
	Field    string `json:"field"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

type ReconcileReport struct {
	Key     string                   `json:"key"`
	Drifts  []Drift                  `json:"drifts"`
	Outbox  store.LinearOutboxStatus `json:"outbox"`
	Body    string                   `json:"body"`
	Changed bool                     `json:"changed"`
}

type Reconciler struct {
	Client        *Client
	ListTasks     func(context.Context) ([]store.Task, error)
	OutboxStatus  func(context.Context) (store.LinearOutboxStatus, error)
	WriteDocument func(context.Context, store.Document) (store.Document, bool, error)
	Interval      time.Duration
	Now           func() time.Time
	DryRun        bool
	Logger        *log.Logger
}

func (reconciler *Reconciler) logf(format string, values ...any) {
	if reconciler.Logger != nil {
		reconciler.Logger.Printf(format, values...)
	}
}

func taskLabels(task store.Task) []string {
	if task.Refs.Linear == nil {
		return nil
	}
	labels := append([]string{}, task.Refs.Linear.Labels...)
	labels = append(labels, task.Lane)
	if task.Refs.Linear.Tier != "" {
		labels = append(labels, task.Refs.Linear.Tier)
	}
	if task.Refs.Linear.Grade != "" {
		labels = append(labels, task.Refs.Linear.Grade)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, label := range labels {
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

func issueLabels(issue Issue) []string {
	labels := make([]string, 0, len(issue.Labels.Nodes))
	for _, label := range issue.Labels.Nodes {
		labels = append(labels, label.Name)
	}
	sort.Strings(labels)
	return labels
}

func missingLabels(expected, actual []string) []string {
	present := map[string]bool{}
	for _, label := range actual {
		present[label] = true
	}
	missing := []string{}
	for _, label := range expected {
		if !present[label] {
			missing = append(missing, label)
		}
	}
	return missing
}

func formatReconcileBody(date string, drifts []Drift, status store.LinearOutboxStatus) string {
	lines := []string{
		"# Linear reconcile " + date,
		"",
		fmt.Sprintf("Outbox pending: %d", status.Pending),
		fmt.Sprintf("Outbox failed: %d", status.Failed),
	}
	if status.LastError != "" {
		lines = append(lines, "Outbox last error: "+status.LastError)
	}
	lines = append(lines, "", fmt.Sprintf("Drifts: %d", len(drifts)))
	for _, drift := range drifts {
		line := fmt.Sprintf("- task %d", drift.TaskID)
		if drift.IssueID != "" {
			line += " issue " + drift.IssueID
		}
		line += ": " + drift.Field
		if drift.Expected != "" || drift.Actual != "" {
			line += fmt.Sprintf(" expected=%q actual=%q", drift.Expected, drift.Actual)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

func (reconciler *Reconciler) RunOnce(ctx context.Context) (ReconcileReport, error) {
	if reconciler.Client == nil || reconciler.ListTasks == nil {
		return ReconcileReport{}, fmt.Errorf("Linear reconciler requires a client and task source")
	}
	now := time.Now().UTC()
	if reconciler.Now != nil {
		now = reconciler.Now().UTC()
	}
	date := now.Format("2006-01-02")
	report := ReconcileReport{Key: "report/linear/reconcile/" + date, Drifts: []Drift{}}
	tasks, err := reconciler.ListTasks(ctx)
	if err != nil {
		return report, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for _, task := range tasks {
		if task.Refs.Linear == nil || !task.Refs.Linear.Sync {
			continue
		}
		issue, found, lookupErr := reconciler.Client.SearchIssue(ctx, markerForTask(task.ID))
		if lookupErr != nil {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "lookup_error", Actual: lookupErr.Error()})
			continue
		}
		if !found {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, Field: "issue", Expected: "present", Actual: "missing"})
			continue
		}
		if issue.Title != task.Title {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, IssueID: issue.ID, Field: "title", Expected: task.Title, Actual: issue.Title})
		}
		mapping, known := store.LinearTaskStateMapping[task.State]
		if !known {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, IssueID: issue.ID, Field: "state_mapping", Actual: "missing"})
		} else if mapping.Mutate && issue.State.Name != mapping.Name {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, IssueID: issue.ID, Field: "state", Expected: mapping.Name, Actual: issue.State.Name})
		}
		archived := issue.ArchivedAt != nil
		if known && mapping.Terminal != archived {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, IssueID: issue.ID, Field: "archived", Expected: fmt.Sprint(mapping.Terminal), Actual: fmt.Sprint(archived)})
		}
		actualLabels := issueLabels(issue)
		if missing := missingLabels(taskLabels(task), actualLabels); len(missing) > 0 {
			report.Drifts = append(report.Drifts, Drift{TaskID: task.ID, IssueID: issue.ID, Field: "labels", Expected: strings.Join(missing, ","), Actual: "missing"})
		}
	}
	if reconciler.OutboxStatus != nil {
		report.Outbox, err = reconciler.OutboxStatus(ctx)
		if err != nil {
			return report, err
		}
	}
	report.Body = formatReconcileBody(date, report.Drifts, report.Outbox)
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
			reconciler.logf("linear reconcile error: %v", err)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

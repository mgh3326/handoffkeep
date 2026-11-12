package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskEventCmd posts one mergewake event by hand (tasks event). The mergewake
// sender is separate work; this command is how the endpoint is exercised.
func taskEventCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks event", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	source := fs.String("source", "", "event source: mergewake.pr, mergewake.review, mergewake.merge or mergewake.revert")
	sourceRef := fs.String("source-ref", "", "the PR URL; for a revert, the revert PR URL")
	pr := fs.String("pr", "", "the PR URL (defaults to --source-ref)")
	revertedPR := fs.String("reverted-pr", "", "the PR a revert undoes (required for mergewake.revert)")
	revertPR := fs.String("revert-pr", "", "the revert PR URL (defaults to --source-ref)")
	at := fs.String("at", "", "when the event happened, RFC 3339 (defaults to now)")
	tasks := fs.String("tasks", "", "other task ids the PR names, comma-separated (one per Mergewake-Task line); each gets the event too")
	ordered, err := splitArgs(args, "source", "source-ref", "pr", "reverted-pr", "revert-pr", "at", "tasks")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: tasks event <task-id> --source <source> --source-ref <pr-url> [--reverted-pr <url>] [--tasks <id,...>]")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if *source == "" || *sourceRef == "" {
		return errors.New("tasks event requires --source and --source-ref")
	}
	in := store.TaskSourceEvent{Source: *source, SourceRef: *sourceRef, TaskID: id, PR: *pr, RevertedPR: *revertedPR, RevertPR: *revertPR}
	if *tasks != "" {
		for _, f := range strings.Split(*tasks, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
			if err != nil || n < 1 {
				return errors.New("--tasks must be positive task ids, comma-separated")
			}
			in.Tasks = append(in.Tasks, n)
		}
	}
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return errors.New("--at must be an RFC 3339 time")
		}
		in.At = &t
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.ApplyTaskEvent(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// taskSweepCmd prints what the backlog sweep would park (tasks sweep
// --dry-run --days N). It writes nothing; the sweep itself runs in serve
// when --backlog-icebox-days is set.
func taskSweepCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks sweep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	dryRun := fs.Bool("dry-run", false, "print what would move and write nothing (required)")
	days := fs.Int("days", 0, "backlog idle days to test")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks sweep takes flags only")
	}
	if !*dryRun {
		return errors.New("tasks sweep runs only with --dry-run; the sweep itself runs in serve with --backlog-icebox-days")
	}
	if *days < 1 || *days > store.MaxBacklogIceboxDays {
		return fmt.Errorf("tasks sweep requires --days N with N in 1..%d", store.MaxBacklogIceboxDays)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	xs, err := c.SweepPreview(ctx, *days)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"dry_run": true, "days": *days, "count": len(xs), "tasks": xs})
}

// validAutomationFlags checks serve's --started-stalled-hours (1..8760) and
// --backlog-icebox-days (0 for off, or 1..3650) against the store's bounds.
func validAutomationFlags(stalledHours, iceboxDays int) error {
	if err := store.CheckStartedStalledHours(stalledHours); err != nil {
		return fmt.Errorf("--started-stalled-hours: %w", err)
	}
	if err := store.CheckBacklogIceboxDays(iceboxDays); err != nil {
		return fmt.Errorf("--backlog-icebox-days: %w", err)
	}
	return nil
}

// backlogSweepWorker runs the backlog sweep when backlog_icebox_days is set.
// Days below 1 is the default and keeps it off (Q7).
type backlogSweepWorker struct {
	store    *store.Store
	days     int
	interval time.Duration
}

const backlogSweepInterval = time.Hour

func (w backlogSweepWorker) Run(ctx context.Context) {
	if w.store == nil || w.days < 1 {
		return
	}
	interval := w.interval
	if interval <= 0 {
		interval = backlogSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		moved, err := w.store.SweepBacklog(ctx, w.days, false)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tasks.sweep_error err=%q", err.Error())
		} else if len(moved) > 0 {
			log.Printf("tasks.sweep moved=%d days=%d", len(moved), w.days)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// lastActivityRepairer is the store call lastActivityRepairWorker needs.
type lastActivityRepairer interface {
	RepairLastActivity(ctx context.Context, limit int) (int, error)
}

// lastActivityRepairWorker runs once at serve start: it repairs
// tasks.last_activity_at left stale by a binary older than v44 (a rollback
// window) in batches until a pass repairs nothing, logs the total, and returns.
type lastActivityRepairWorker struct {
	store lastActivityRepairer
}

const lastActivityRepairBatch = 500

func (w lastActivityRepairWorker) Run(ctx context.Context) {
	total := 0
	for ctx.Err() == nil {
		n, err := w.store.RepairLastActivity(ctx, lastActivityRepairBatch)
		total += n
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tasks.last_activity_repair_error err=%q", err.Error())
			return
		}
		if n == 0 {
			log.Printf("tasks.last_activity_repair repaired=%d", total)
			return
		}
	}
}

// taskDriftCmd prints the drift report (tasks drift): the tasks whose state
// disagrees with the records around them. It writes nothing. Text by default;
// --json prints the report as JSON.
func taskDriftCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks drift", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	mergedDays := fs.Int("merged-days", store.DriftDefaultMergedDays, "report merged tasks older than this many days that no deploy record lists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks drift takes flags only")
	}
	if err := store.CheckDriftMergedDays(*mergedDays); err != nil {
		return fmt.Errorf("--merged-days: %w", err)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := c.TasksDrift(ctx, *mergedDays)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, rep)
	}
	return writeTaskDriftText(out, rep)
}

// taskSettleMergedCmd runs the merged-task sweep (tasks settle-merged). It
// defaults to --dry-run: the report lists what would move and writes nothing.
// --dry-run=false applies the moves. Output is JSON.
func taskSettleMergedCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks settle-merged", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	project := fs.String("project", "", "settle only this project's merged tasks (default every project)")
	dryRun := fs.Bool("dry-run", true, "print what would move and write nothing; --dry-run=false applies")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks settle-merged takes flags only")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, err := c.SettleMerged(ctx, *project, *dryRun)
	if err != nil {
		return err
	}
	return printJSON(out, rep)
}

// writeTaskDriftText prints one heading per drift class, then one line per
// task it names.
func writeTaskDriftText(out io.Writer, rep store.TaskDrift) error {
	var b strings.Builder
	fmt.Fprintf(&b, "tasks drift as of %s (merged_days=%d, started_stalled_hours=%d)\n", rep.AsOf.Format(time.RFC3339), rep.MergedDays, rep.StartedStalledHours)
	classes := []struct {
		name string
		c    store.TaskDriftClass
	}{
		{"settled_open_decision", rep.SettledOpenDecision},
		{"merged_pr_not_merged", rep.MergedPRNotMerged},
		{"merged_without_deploy", rep.MergedWithoutDeploy},
		{"idle_started", rep.IdleStarted},
	}
	for _, k := range classes {
		fmt.Fprintf(&b, "\n%s: %d (%s)", k.name, k.c.Count, k.c.Rule)
		if k.c.Truncated {
			fmt.Fprintf(&b, " — more candidates than the %d listed", len(k.c.Tasks))
		}
		b.WriteString("\n")
		for _, x := range k.c.Tasks {
			fmt.Fprintf(&b, "  #%d %s %s lane=%s since=%s", x.ID, x.Kind, x.State, x.Lane, x.Since.Format(time.RFC3339))
			if x.PR != "" {
				fmt.Fprintf(&b, " pr=%s", x.PR)
			}
			if x.DecisionRequest != "" {
				fmt.Fprintf(&b, " decision_request=%s", x.DecisionRequest)
			}
			fmt.Fprintf(&b, " %s\n", x.Title)
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

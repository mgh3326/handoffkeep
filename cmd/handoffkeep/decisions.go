package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const decisionListUsage = "usage: decisions list [--status open|answered|superseded|expired] [--lane <lane>] [--source <source>] [--include-snoozed] [--limit N]"
const decisionSnoozeUsage = "usage: decisions snooze <id> --until <RFC3339>"
const decisionBackfillUsage = "usage: decisions backfill --dry-run [--json]"
const decisionHygieneUsage = "usage: decisions hygiene"

// decisionListCmd prints decision records as JSON, oldest first.
func decisionListCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var f store.DecisionRecordFilter
	fs.StringVar(&f.Status, "status", "", "open (default), answered, superseded or expired")
	fs.StringVar(&f.Lane, "lane", "", "only this lane")
	fs.StringVar(&f.Source, "source", "", "only this source")
	fs.BoolVar(&f.IncludeSnoozed, "include-snoozed", false, "list snoozed records too")
	fs.IntVar(&f.Limit, "limit", 0, "maximum results (server default 1000)")
	parsed, err := splitArgs(args, "status", "lane", "source", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 0 {
		return errors.New(decisionListUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := c.ListDecisionRecords(ctx, f)
	if err != nil {
		return err
	}
	return printJSON(out, list)
}

// decisionSnoozeCmd hides one open record from the default list until a time.
func decisionSnoozeCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions snooze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	until := fs.String("until", "", "RFC3339 time the record is hidden until")
	parsed, err := splitArgs(args, "until")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 1 || *until == "" {
		return errors.New(decisionSnoozeUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("decision request id must be positive")
	}
	at, err := time.Parse(time.RFC3339, *until)
	if err != nil {
		return errors.New("--until must be an RFC3339 time")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, err := c.SnoozeDecisionRecord(ctx, id, at)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

// decisionBackfillCmd classifies every question the sources count as open.
// It runs only as a dry run, over read-only API calls: it prints one row per
// question and the totals, and writes nothing. --json prints the same report
// as JSON.
func decisionBackfillCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions backfill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	dryRun := fs.Bool("dry-run", false, "classify only; required")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	parsed, err := splitArgs(args)
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 0 {
		return errors.New(decisionBackfillUsage)
	}
	if !*dryRun {
		return errors.New("decisions backfill only runs as --dry-run: closing an open question needs the operator's explicit OK")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src, err := c.OpenDecisionSources(ctx)
	if err != nil {
		return err
	}
	report := store.ClassifyOpenDecisions(src)
	if *asJSON {
		return printJSON(out, report)
	}
	return printDecisionBackfill(out, report)
}

// decisionHygieneCmd prints the decision delivery counts per destination.
func decisionHygieneCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions hygiene", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	parsed, err := splitArgs(args)
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 0 {
		return errors.New(decisionHygieneUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := c.DecisionDeliveryHygiene(ctx)
	if err != nil {
		return err
	}
	return printJSON(out, h)
}

func printDecisionBackfill(out io.Writer, r store.DecisionBackfillReport) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tSOURCE_REF\tLANE\tAGE\tTASK\tTASK_STATE\tJOB\tJOB_STATE\tCLASS")
	for _, x := range r.Rows {
		task := "-"
		if x.TaskID != nil {
			task = "#" + strconv.FormatInt(*x.TaskID, 10)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", x.Source, x.SourceRef, dash(x.Lane), decisionAge(x.AgeHours), task, dash(x.TaskState), dash(x.JobID), dash(x.JobState), x.Class)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	parts := []string{}
	for _, c := range store.DecisionClasses() {
		parts = append(parts, fmt.Sprintf("%s=%d", c, r.ByClass[c]))
	}
	fmt.Fprintln(out, "by class: "+strings.Join(parts, " "))
	parts = parts[:0]
	for _, s := range store.DecisionSources() {
		parts = append(parts, fmt.Sprintf("%s=%d", s, r.BySource[s]))
	}
	fmt.Fprintln(out, "by source: "+strings.Join(parts, " "))
	fmt.Fprintf(out, "total: %d (dry run, server time %s; nothing was written)\n", r.Total, r.ServerTime.UTC().Format(time.RFC3339))
	if len(r.Truncated) > 0 {
		fmt.Fprintf(out, "truncated: %s hit the read limit; their counts are lower bounds\n", strings.Join(r.Truncated, ", "))
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func decisionAge(hours int64) string {
	if hours < 24 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd%dh", hours/24, hours%24)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	ledgerUsage        = "usage: handoffkeep ledger [--consumer desk] [--json] | handoffkeep ledger ack [--consumer desk] <sync_id>"
	ledgerAckUsage     = "usage: handoffkeep ledger ack [--consumer desk] <sync_id>"
	sinceUsage         = "usage: handoffkeep since [--consumer desk] [--json]"
	projectStatusUsage = "usage: handoffkeep tasks project-status <project> --health on_track|at_risk|off_track --body <text> | handoffkeep tasks project-status <project> [--limit N]"
	defaultConsumer    = "desk"
)

// splitFlagArgs puts flags before positionals so either order parses. Value
// flags are named without dashes and match in their -x and --x forms; a
// -x=v argument carries its own value.
func splitFlagArgs(args []string, valueFlags ...string) ([]string, error) {
	takes := map[string]bool{"url": true, "token": true}
	for _, f := range valueFlags {
		takes[f] = true
	}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if name := strings.TrimLeft(arg, "-"); takes[name] {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...), nil
}

// ledgerCmd prints the desk ledger: text by default, sections in order, or
// the API response with --json. "ledger ack" moves the cursor.
func ledgerCmd(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "ack" {
		return ledgerAckCmd(args[1:], out)
	}
	return ledgerReadCmd("ledger", args, out, false)
}

// sinceCmd prints only section 5, the change-feed delta from the cursor.
func sinceCmd(args []string, out io.Writer) error {
	return ledgerReadCmd("since", args, out, true)
}

func ledgerReadCmd(name string, args []string, out io.Writer, onlyChanges bool) error {
	usage := ledgerUsage
	if onlyChanges {
		usage = sinceUsage
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	consumer := fs.String("consumer", defaultConsumer, "ledger consumer whose cursor is read")
	asJSON := fs.Bool("json", false, "print JSON")
	parsed, err := splitFlagArgs(args, "consumer")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 0 || *consumer == "" {
		return errors.New(usage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := c.Ledger(ctx, *consumer)
	if err != nil {
		return ledgerReadError(err, *consumer)
	}
	switch {
	case *asJSON && onlyChanges:
		return printJSON(out, l.ChangedSinceAck)
	case *asJSON:
		return printJSON(out, l)
	case onlyChanges:
		return writeLedgerChanges(out, l, true)
	}
	return writeLedger(out, l)
}

// ledgerReadError explains a cursor the feed has pruned past.
func ledgerReadError(err error, consumer string) error {
	var reset *store.SyncReset
	if errors.As(err, &reset) {
		return fmt.Errorf("the %s cursor is older than the change feed keeps (oldest sync id %d): read the current view, then ack it with handoffkeep ledger ack --consumer %s <sync_id>: %w", consumer, reset.MinSyncID, consumer, err)
	}
	return err
}

// ledgerAckCmd moves a consumer's cursor forward and prints it as JSON.
func ledgerAckCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ledger ack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	consumer := fs.String("consumer", defaultConsumer, "ledger consumer whose cursor moves")
	parsed, err := splitFlagArgs(args, "consumer")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 1 || *consumer == "" {
		return errors.New(ledgerAckUsage)
	}
	syncID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || syncID < 0 {
		return errors.New("sync_id must be a nonnegative integer")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cur, err := c.AckLedger(ctx, *consumer, syncID)
	if err != nil {
		return ledgerReadError(err, *consumer)
	}
	return printJSON(out, cur)
}

// projectStatusCmd posts a project status update with --health and --body,
// or lists the project's updates without them.
func projectStatusCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks project-status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	health := fs.String("health", "", "on_track, at_risk or off_track")
	body := fs.String("body", "", "the update text")
	limit := fs.Int("limit", 0, "maximum updates listed (server default 100)")
	parsed, err := splitFlagArgs(args, "health", "body", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 1 || *limit < 0 {
		return errors.New(projectStatusUsage)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	create := set["health"] || set["body"]
	if create && (!set["health"] || !set["body"] || set["limit"]) {
		return errors.New(projectStatusUsage)
	}
	if create && !store.ValidHealth(*health) {
		return errors.New("--health must be on_track, at_risk or off_track")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if create {
		x, err := c.CreateStatusUpdate(ctx, fs.Arg(0), *health, *body)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	}
	list, err := c.ListStatusUpdates(ctx, fs.Arg(0), *limit)
	if err != nil {
		return err
	}
	return printJSON(out, list)
}

// ledgerAge renders a duration as its two largest units: 3d4h, 5h12m, 45m, 30s.
func ledgerAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	days := d / (24 * time.Hour)
	hours := (d % (24 * time.Hour)) / time.Hour
	minutes := (d % time.Hour) / time.Minute
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// ledgerLine keeps one line of free text short.
func ledgerLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func ledgerProject(x store.LedgerTask) string {
	if x.Project == nil || *x.Project == "" {
		return "-"
	}
	return *x.Project
}

func writeLedgerTask(b *strings.Builder, x store.LedgerStartedTask, now time.Time, stalled bool) {
	fmt.Fprintf(b, "   #%d %s [%s] %s", x.ID, x.State, ledgerProject(x.LedgerTask), ledgerLine(x.Title, 80))
	if stalled {
		fmt.Fprintf(b, " — idle %s", ledgerAge(now.Sub(x.ActivityAt)))
	} else if x.LastEvent != nil {
		ev := x.LastEvent.Kind
		if x.LastEvent.Source != "" {
			ev += " " + x.LastEvent.Source
		}
		fmt.Fprintf(b, " — last %s %s ago", ev, ledgerAge(now.Sub(x.LastEvent.At)))
	} else {
		fmt.Fprintf(b, " — no events, active %s ago", ledgerAge(now.Sub(x.ActivityAt)))
	}
	if x.NextTrigger != "" {
		fmt.Fprintf(b, "; next: %s (%s)", x.NextTrigger, x.NextSource)
	}
	b.WriteString("\n")
}

// writeLedger renders the five sections in order.
func writeLedger(out io.Writer, l store.Ledger) error {
	var b strings.Builder
	now := l.At
	fmt.Fprintf(&b, "Ledger for %s at %s (stalled after %dh)\n\n", l.Consumer, now.Format(time.RFC3339), l.StalledAfterHours)

	fmt.Fprintf(&b, "1. In flight (%d)\n", len(l.InFlight.Tasks))
	for _, p := range l.InFlight.Projects {
		fmt.Fprintf(&b, "   project %s: %d started, ", p.Project, p.Started)
		if p.LastUpdateAt == nil {
			b.WriteString("no status update")
		} else {
			fmt.Fprintf(&b, "%s %s ago", p.Health, ledgerAge(now.Sub(*p.LastUpdateAt)))
		}
		if p.Stale {
			b.WriteString(" (stale)")
		}
		b.WriteString("\n")
	}
	for _, x := range l.InFlight.Tasks {
		writeLedgerTask(&b, x, now, false)
	}

	fmt.Fprintf(&b, "\n2. Waiting on operator (%d)\n", l.WaitingOnOperator.Count)
	for _, g := range l.WaitingOnOperator.Groups {
		switch {
		case g.Task != nil:
			fmt.Fprintf(&b, "   #%d %s %s\n", g.Task.ID, g.Task.State, ledgerLine(g.Task.Title, 80))
		case g.TaskID != nil:
			fmt.Fprintf(&b, "   #%d (task not found)\n", *g.TaskID)
		default:
			b.WriteString("   no task\n")
		}
		for _, r := range g.Requests {
			fmt.Fprintf(&b, "     request %d %s %s ago", r.ID, r.Source, ledgerAge(now.Sub(r.CreatedAt)))
			if r.Overdue {
				b.WriteString(" overdue")
			}
			fmt.Fprintf(&b, ": %s\n", ledgerLine(r.Question, 100))
		}
	}

	fmt.Fprintf(&b, "\n3. Stalled (%d tasks, %d events)\n", len(l.Stalled.Tasks), len(l.Stalled.Events))
	for _, x := range l.Stalled.Tasks {
		writeLedgerTask(&b, x, now, true)
	}
	for _, e := range l.Stalled.Events {
		fmt.Fprintf(&b, "   event %d %s on #%d %s %s, %s ago: %s\n", e.EventID, e.Source, e.Task.ID, e.Task.State, ledgerLine(e.Task.Title, 60), ledgerAge(now.Sub(e.At)), e.SourceRef)
	}

	fmt.Fprintf(&b, "\n4. Triage (%d", l.Triage.Count)
	if l.Triage.OldestAt != nil {
		fmt.Fprintf(&b, ", oldest %s", ledgerAge(time.Duration(l.Triage.OldestAgeSeconds)*time.Second))
	}
	b.WriteString(")\n")
	for _, x := range l.Triage.Tasks {
		fmt.Fprintf(&b, "   #%d [%s] %s (%s)\n", x.ID, ledgerProject(x.LedgerTask), ledgerLine(x.Title, 80), ledgerAge(time.Duration(x.AgeSeconds)*time.Second))
	}
	b.WriteString("\n")
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	return writeLedgerChanges(out, l, false)
}

// writeLedgerChanges renders section 5. withRows lists the feed rows too
// (handoffkeep since); the ledger shows the summary only.
func writeLedgerChanges(out io.Writer, l store.Ledger, withRows bool) error {
	var b strings.Builder
	ch := l.ChangedSinceAck
	if ch.NoCursor || ch.Cursor == nil {
		fmt.Fprintf(&b, "5. Changed since last ack: no cursor yet for %s (feed at %d). Start one with handoffkeep ledger ack --consumer %s %d\n", l.Consumer, ch.MaxSyncID, l.Consumer, ch.MaxSyncID)
		_, err := io.WriteString(out, b.String())
		return err
	}
	fmt.Fprintf(&b, "5. Changed since last ack (%d changes after %d, feed at %d)\n", ch.Total, ch.Cursor.AckedSyncID, ch.MaxSyncID)
	for _, x := range ch.Summary {
		fmt.Fprintf(&b, "   %s %s %d\n", x.Model, x.Action, x.Count)
	}
	if withRows {
		for _, r := range ch.Rows {
			fmt.Fprintf(&b, "   %d %s %s %s %s\n", r.SyncID, r.Model, r.ModelID, r.Action, r.At.UTC().Format(time.RFC3339))
		}
	}
	if ch.Total > 0 {
		shown := "the first"
		if !ch.Truncated {
			shown = "all"
		}
		fmt.Fprintf(&b, "   %s %d rows end at %d; ack with handoffkeep ledger ack --consumer %s %d\n", shown, len(ch.Rows), ch.LastSyncID, l.Consumer, ch.LastSyncID)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

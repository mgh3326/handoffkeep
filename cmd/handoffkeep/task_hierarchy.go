package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskParentCmd sets or clears a task's single parent.
//
//	tasks parent <id> --to <parent-id>
//	tasks parent <id> --clear
func taskParentCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks parent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	to := fs.Int64("to", 0, "parent task id")
	clear := fs.Bool("clear", false, "remove the task's parent")
	ordered, err := splitArgs(args, "to")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks parent requires a task id")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if (*to > 0) == *clear {
		return errors.New("tasks parent requires exactly one of --to <parent-id> and --clear")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var parent *int64
	if !*clear {
		parent = to
	}
	x, err := c.SetTaskParent(ctx, id, parent)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

// taskRelationCmd adds (relate) or archives (unrelate) a relation edge.
//
//	tasks relate <id> --kind blocks|duplicate_of|related --other <id>
//	tasks unrelate <id> --kind ... --other <id>
//
// blocks: <id> blocks <other>. duplicate_of: <id> is a duplicate of <other>
// and the task enters duplicate in the same step.
func taskRelationCmd(args []string, out io.Writer) error {
	name := "tasks " + args[0]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	kind := fs.String("kind", "", "relation kind: "+strings.Join(store.RelationKinds(), ", "))
	other := fs.Int64("other", 0, "task id at the other end of the relation")
	ordered, err := splitArgs(args[1:], "kind", "other")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(name + " requires a task id")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if *kind == "" || *other < 1 {
		return fmt.Errorf("%s requires --kind (%s) and --other <task id>", name, strings.Join(store.RelationKinds(), ", "))
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res interface{}
	if args[0] == "relate" {
		res, err = c.AddTaskRelation(ctx, id, *kind, *other)
	} else {
		res, err = c.RemoveTaskRelation(ctx, id, *kind, *other)
	}
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// taskMigrateStatesCmd is the pre-deploy dry-run of the state migration. It
// reads the database directly (HANDOFFKEEP_DB_URL, the variable serve uses):
// the server it would ask is the one that has not been upgraded yet. It opens
// the pool without migrating, runs one read-only transaction and writes
// nothing. Only --dry-run exists: the migration itself runs at server start.
func taskMigrateStatesCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks migrate-states", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print the counts the migration would produce and write nothing")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks migrate-states takes flags only")
	}
	if !*dryRun {
		return errors.New("tasks migrate-states supports only --dry-run: the migration runs when the upgraded server starts")
	}
	dbURL := os.Getenv("HANDOFFKEEP_DB_URL")
	if dbURL == "" {
		return errors.New("HANDOFFKEEP_DB_URL is required: the dry-run reads the database the server will migrate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.OpenForPlan(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	plan, err := st.PlanStateMigration(ctx)
	if err != nil {
		return err
	}
	return writeStateMigrationPlan(out, plan, *asJSON)
}

func writeStateMigrationPlan(out io.Writer, plan store.StateMigrationPlan, asJSON bool) error {
	if asJSON {
		return printJSON(out, plan)
	}
	if plan.Migrated {
		fmt.Fprintln(out, "already migrated: no legacy state is left to plan")
		return nil
	}
	fmt.Fprintf(out, "%-16s %-22s %-12s %s\n", "legacy_state", "rule", "new_state", "count")
	total := 0
	for _, c := range plan.Counts {
		fmt.Fprintf(out, "%-16s %-22s %-12s %d\n", c.LegacyState, c.Rule, c.NewState, c.Count)
		total += c.Count
	}
	fmt.Fprintf(out, "changed tasks: %d (one sync_log row and one state_migration event each)\n", total)
	fmt.Fprintf(out, "hold tasks mirrored to Linear that would show as Canceled: %d\n", plan.HoldMirroredToCanceled)
	return nil
}

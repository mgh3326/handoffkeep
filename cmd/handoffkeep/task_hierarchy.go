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

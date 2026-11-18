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

// taskWaitCmd is `tasks wait <id> --on <kind>[:<ref>] --for <text>` and
// `tasks unwait <id> <wait> [--note <text>]` (hk 5128). A wait is a session's
// hand wait: the store derives the rest.
func taskWaitCmd(args []string, out io.Writer) error {
	name := "tasks " + args[0]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	if args[0] == "wait" {
		on := fs.String("on", "", "what the task waits on: operator, lane:<name>, task:<id>, pr:<ref>, ci:<ref> or external:<name>")
		reason := fs.String("for", "", "what is wanted, in one line")
		ordered, err := splitArgs(args[1:], "on", "for")
		if err != nil {
			return err
		}
		if err := fs.Parse(ordered); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(name + " requires a task id")
		}
		id, err := parseTaskIDArg(fs.Arg(0))
		if err != nil {
			return err
		}
		if *on == "" || *reason == "" {
			return fmt.Errorf("%s requires --on (operator, lane:<name>, task:<id>, pr:<ref>, ci:<ref>, external:<name>) and --for <text>", name)
		}
		kind, ref, _ := strings.Cut(*on, ":")
		if err := mustClient(c); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := c.AddTaskWait(ctx, id, store.TaskWaitInput{OnKind: kind, OnRef: ref, Reason: *reason})
		if err != nil {
			return err
		}
		return printJSON(out, res)
	}
	note := fs.String("note", "", "why the wait ended, in one line")
	ordered, err := splitArgs(args[1:], "note")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%s requires a task id and a wait id", name)
	}
	id, err := parseTaskIDArg(fs.Arg(0))
	if err != nil {
		return err
	}
	waitID, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil || waitID < 1 {
		return errors.New("wait id must be positive")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.ClearTaskWait(ctx, id, waitID, *note)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// parseTaskIDArg reads a positive task id argument.
func parseTaskIDArg(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("task id must be positive")
	}
	return id, nil
}

// waitsCmd is `waits list [--section <key>]`: the waits sections and the
// ordinary backlog, as JSON (hk 5128). The section keys are needs_operator,
// failed_start, waiting_on and idle_started.
func waitsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("usage: waits list [--section needs_operator|failed_start|waiting_on|idle_started]")
	}
	fs := flag.NewFlagSet("waits list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	section := fs.String("section", "", "one section: needs_operator, failed_start, waiting_on or idle_started")
	ordered, err := splitArgs(args[1:], "section")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("waits list takes no positional arguments")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.ListWaits(ctx, *section)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

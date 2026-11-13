package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"
)

const setPriorityUsage = "usage: tasks set-priority <id> <n> [--note <reason>]"

// resolveSetPriority turns the positional arguments of tasks set-priority into
// the task id and priority integer.
func resolveSetPriority(positional []string) (int64, int, error) {
	if len(positional) != 2 {
		return 0, 0, errors.New(setPriorityUsage)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id < 1 {
		return 0, 0, errors.New("task id must be positive")
	}
	prio, err := strconv.Atoi(positional[1])
	if err != nil || prio < 0 {
		return 0, 0, errors.New("priority must be a non-negative integer")
	}
	return id, prio, nil
}

// taskPriorityCmd serves "tasks set-priority <id> <n> [--note <reason>]" (hk 3219).
func taskPriorityCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks set-priority", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	note := fs.String("note", "", "reason recorded on the priority event")
	ordered, err := splitArgs(args[1:], "note")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	id, prio, err := resolveSetPriority(fs.Args())
	if err != nil {
		return err
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, err := c.SetTaskPriority(ctx, id, prio, *note)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

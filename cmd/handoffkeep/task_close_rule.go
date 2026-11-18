package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"
)

const setCloseRuleUsage = "usage: tasks close-rule <id> auto|explicit [--note <reason>]"

// resolveCloseRule turns the positional arguments of tasks close-rule into the
// task id and the rule. The rule is checked by the store, which answers
// invalid_close_rule for anything but auto or explicit.
func resolveCloseRule(positional []string) (int64, string, error) {
	if len(positional) != 2 {
		return 0, "", errors.New(setCloseRuleUsage)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id < 1 {
		return 0, "", errors.New("task id must be positive")
	}
	return id, positional[1], nil
}

// taskCloseRuleCmd serves "tasks close-rule <id> auto|explicit [--note <reason>]" (hk 5112).
func taskCloseRuleCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks close-rule", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	note := fs.String("note", "", "reason recorded on the close-rule event")
	ordered, err := splitArgs(args[1:], "note")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	id, rule, err := resolveCloseRule(fs.Args())
	if err != nil {
		return err
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, err := c.SetTaskCloseRule(ctx, id, rule, *note)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

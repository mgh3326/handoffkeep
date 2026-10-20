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
)

func taskRebindCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks rebind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	fromJob := fs.String("from-job", "", "current task job id that is being handed off")
	toJob := fs.String("to-job", "", "new task job id")
	by := fs.String("by", "", "new builder label")
	reason := fs.String("reason", "", "handoff reason recorded in task history")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--from-job": true, "--to-job": true, "--by": true, "--reason": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if valueFlags[arg] {
			i++
			if i >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks rebind requires a task id")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if *fromJob == "" || *toJob == "" || *by == "" || strings.TrimSpace(*reason) == "" {
		return errors.New("tasks rebind requires --from-job, --to-job, --by and --reason")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, err := c.RebindTaskJob(ctx, id, *fromJob, *toJob, *by, *reason)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

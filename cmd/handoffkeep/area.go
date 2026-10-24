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
	setAreaUsage = "usage: tasks set-area <id> <area> [--note <reason>] | tasks set-area <id> --clear [--note <reason>]"
	areasUsage   = "usage: tasks areas"
)

// resolveSetArea turns the positional arguments of tasks set-area into the
// task id and the area to write; a nil area means clear. Every refusal is
// local, so a malformed command never reaches the server.
func resolveSetArea(positional []string, clearArea bool) (int64, *string, error) {
	want := 2
	if clearArea {
		want = 1
	}
	if len(positional) != want {
		if clearArea && len(positional) == 2 {
			return 0, nil, errors.New("--clear takes no area value; " + setAreaUsage)
		}
		return 0, nil, errors.New(setAreaUsage)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id < 1 {
		return 0, nil, errors.New("task id must be positive")
	}
	if clearArea {
		return id, nil, nil
	}
	area := positional[1]
	if err := store.ValidateTaskArea(&area); err != nil {
		return 0, nil, err
	}
	return id, &area, nil
}

// taskAreaCmd serves "tasks set-area" and "tasks areas". set-area is the
// SetTaskProject twin: one recorded kind='area' event per change, with the
// actor taken from the bearer token. "tasks areas" prints the closed
// vocabulary the server holds, in order.
func taskAreaCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	note := fs.String("note", "", "reason recorded on the area event")
	clearArea := fs.Bool("clear", false, "remove the task's area instead of setting one")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--note": true}
	flags, positional := []string{}, []string{}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if valueFlags[arg] {
			i++
			if i >= len(rest) {
				return fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, rest[i])
		}
	}
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}
	if args[0] == "areas" {
		if fs.NArg() != 0 || *clearArea || *note != "" {
			return errors.New(areasUsage)
		}
	}
	if args[0] == "set-area" {
		if _, _, err := resolveSetArea(fs.Args(), *clearArea); err != nil {
			return err
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if args[0] == "areas" {
		areas, err := c.ListTaskAreas(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"areas": areas})
	}
	id, area, _ := resolveSetArea(fs.Args(), *clearArea)
	x, err := c.SetTaskArea(ctx, id, area, *note)
	if err != nil {
		return err
	}
	return printJSON(out, x)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
)

const (
	ownersUsage = "usage: tasks owners"
	ownerUsage  = "usage: tasks owner <project> [--set <lane> [--note <text>] | --clear [--note <text>]]"
)

// taskOwnerCmd serves "tasks owners" and "tasks owner". "owners" lists every
// project's design session; "owner <project>" shows one with its history,
// while --set and --clear record an operator decision (the note carries the
// decision doc key). The server stamps the actor from the bearer token.
func taskOwnerCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	set := fs.String("set", "", "design session lane that owns the project")
	clear := fs.Bool("clear", false, "clear the project's design session (falls back to operator-desk)")
	note := fs.String("note", "", "reason recorded on the owner change (for example the decision doc key)")
	ordered, err := splitArgs(args[1:], "set", "note")
	if err != nil {
		return err
	}
	if err = fs.Parse(ordered); err != nil {
		return err
	}
	present := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { present[f.Name] = true })
	if err = mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "owners":
		if fs.NArg() != 0 {
			return errors.New(ownersUsage)
		}
		owners, err := c.ListProjectOwners(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"projects": owners})
	case "owner":
		if fs.NArg() != 1 {
			return errors.New(ownerUsage)
		}
		project := fs.Arg(0)
		if present["set"] && *clear {
			return errors.New("tasks owner takes --set or --clear, not both")
		}
		if *clear {
			owner, err := c.SetProjectDesignLane(ctx, project, "", *note)
			if err != nil {
				return err
			}
			return printJSON(out, owner)
		}
		if present["set"] {
			if *set == "" {
				return errors.New("--set requires a lane; use --clear to clear the owner")
			}
			owner, err := c.SetProjectDesignLane(ctx, project, *set, *note)
			if err != nil {
				return err
			}
			return printJSON(out, owner)
		}
		if present["note"] {
			return errors.New("--note is recorded with --set or --clear")
		}
		detail, err := c.GetProjectOwner(ctx, project)
		if err != nil {
			if remote.ErrorCode(err) == "not_found" {
				return fmt.Errorf("no such project: %s", project)
			}
			return err
		}
		return printJSON(out, detail)
	default:
		return errors.New(ownerUsage)
	}
}

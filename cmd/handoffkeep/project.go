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
	projectUsage  = "usage: tasks project <id> <project> [--note <reason>]"
	projectsUsage = "usage: tasks projects [add <name>]"
)

// taskProjectCmd serves "tasks project" and "tasks projects". "tasks project
// <id> <p>" is the relane-like reclassification recorded as a kind='project'
// event; "tasks projects" lists the server-configured vocabulary and
// "tasks projects add <name>" extends it. There is no actor flag: the server
// takes the event's "by" from the bearer token identity.
func taskProjectCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	note := fs.String("note", "", "reason recorded on the project event")
	status := fs.String("status", "", "project status")
	start := fs.String("start", "", "project start time (RFC3339, or empty to clear)")
	target := fs.String("target", "", "project target time (RFC3339, or empty to clear)")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--note": true, "--status": true, "--start": true, "--target": true}
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
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "project":
		if fs.NArg() != 2 {
			return errors.New(projectUsage)
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("task id must be positive")
		}
		present := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { present[f.Name] = true })
		if present["status"] || present["start"] || present["target"] {
			p := store.ProjectMetadataPatch{SetStart: present["start"], SetTarget: present["target"]}
			if present["status"] {
				p.Status = status
			}
			for _, entry := range []struct {
				name, value string
				dest        **time.Time
			}{{"start", *start, &p.StartAt}, {"target", *target, &p.TargetAt}} {
				if !present[entry.name] {
					continue
				}
				*entry.dest = nil
				if entry.value != "" {
					t, err := time.Parse(time.RFC3339, entry.value)
					if err != nil {
						return err
					}
					*entry.dest = &t
				}
			}
			x, err := c.SetTaskProjectWithMetadata(ctx, id, fs.Arg(1), *note, p)
			if err != nil {
				return err
			}
			return printJSON(out, x)
		}
		x, err := c.SetTaskProject(ctx, id, fs.Arg(1), *note)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "projects":
		if fs.NArg() == 0 {
			names, err := c.ListTaskProjects(ctx)
			if err != nil {
				return err
			}
			return printJSON(out, map[string]any{"projects": names})
		}
		if fs.NArg() == 2 && fs.Arg(0) == "add" {
			created, err := c.AddTaskProject(ctx, fs.Arg(1))
			if err != nil {
				return err
			}
			return printJSON(out, map[string]any{"name": fs.Arg(1), "created": created})
		}
		return errors.New(projectsUsage)
	default:
		return errors.New(projectsUsage)
	}
}

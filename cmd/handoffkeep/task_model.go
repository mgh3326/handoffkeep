package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func taskModelCmd(args []string, out io.Writer) error {
	command := args[0]
	fs := flag.NewFlagSet("tasks "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	to := fs.Int64("to", 0, "milestone or label id")
	clear := fs.Bool("clear", false, "clear field")
	remove := fs.Bool("remove", false, "detach label")
	source := fs.String("source", "cli", "subscription source")
	project := fs.String("project", "", "milestone project")
	description := fs.String("description", "", "milestone description")
	target := fs.String("target", "", "milestone target time")
	status := fs.String("status", "", "milestone status")
	order := fs.Int("order", 0, "milestone sort order")
	color := fs.String("color", "", "label color")
	parent := fs.Int64("parent", 0, "label group id")
	group := fs.Bool("group", false, "label is a group")
	level := fs.String("edit-level", "anyone", "label edit level")
	ordered, err := taskArgsFirst(args[1:], map[string]bool{"--url": true, "--token": true, "--to": true, "--source": true, "--project": true, "--description": true, "--target": true, "--status": true, "--order": true, "--color": true, "--parent": true, "--edit-level": true})
	if err != nil {
		return err
	}
	if err = fs.Parse(ordered); err != nil {
		return err
	}
	if err = mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if command == "labels" {
		if fs.NArg() != 0 {
			return errors.New("usage: tasks labels")
		}
		labels, err := c.ListLabels(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"labels": labels})
	}
	if command == "milestone" && fs.NArg() >= 1 && (fs.Arg(0) == "create" || fs.Arg(0) == "update") {
		update := fs.Arg(0) == "update"
		if (!update && fs.NArg() != 2) || (update && fs.NArg() != 3) {
			return errors.New("usage: tasks milestone create <name> --project <project> | update <id> <name> --project <project>")
		}
		m := store.ProjectMilestone{Project: *project, Description: *description, Status: *status, SortOrder: *order}
		if update {
			id, e := strconv.ParseInt(fs.Arg(1), 10, 64)
			if e != nil || id < 1 {
				return errors.New("milestone id must be positive")
			}
			m.ID = id
			m.Name = fs.Arg(2)
		} else {
			m.Name = fs.Arg(1)
		}
		if *target != "" {
			t, e := time.Parse(time.RFC3339, *target)
			if e != nil {
				return e
			}
			m.TargetAt = &t
		}
		result, e := c.SaveMilestone(ctx, m)
		if e != nil {
			return e
		}
		return printJSON(out, result)
	}
	if command == "label" && fs.NArg() >= 1 && (fs.Arg(0) == "create" || fs.Arg(0) == "retire") {
		if fs.NArg() != 2 {
			return errors.New("usage: tasks label create <name> | retire <id>")
		}
		if fs.Arg(0) == "retire" {
			id, e := strconv.ParseInt(fs.Arg(1), 10, 64)
			if e != nil || id < 1 {
				return errors.New("label id must be positive")
			}
			if e = c.RetireLabel(ctx, id); e != nil {
				return e
			}
			return printJSON(out, map[string]any{"id": id, "retired": true})
		}
		l := store.Label{Name: fs.Arg(1), Color: *color, IsGroup: *group, EditLevel: *level}
		if *parent > 0 {
			l.ParentID = parent
		}
		result, e := c.SaveLabel(ctx, l)
		if e != nil {
			return e
		}
		return printJSON(out, result)
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("tasks %s requires a task id", command)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	var result store.Task
	switch command {
	case "milestone":
		if fs.NArg() != 1 || (*to > 0) == *clear {
			return errors.New("usage: tasks milestone <task-id> --to <milestone-id> | --clear")
		}
		var m *int64
		if !*clear {
			m = to
		}
		result, err = c.SetTaskMilestone(ctx, id, m)
	case "label":
		if fs.NArg() != 1 || *to < 1 {
			return errors.New("usage: tasks label <task-id> --to <label-id> [--remove]")
		}
		result, err = c.SetTaskLabel(ctx, id, *to, !*remove)
	case "due":
		if (fs.NArg() != 2 || *clear) && (fs.NArg() != 1 || !*clear) {
			return errors.New("usage: tasks due <task-id> <RFC3339 time> | --clear")
		}
		var due *time.Time
		if !*clear {
			t, e := time.Parse(time.RFC3339, fs.Arg(1))
			if e != nil {
				return e
			}
			due = &t
		}
		result, err = c.SetTaskDue(ctx, id, due)
	case "subscribe", "unsubscribe":
		if fs.NArg() != 2 {
			return errors.New("usage: tasks subscribe|unsubscribe <task-id> <subscriber>")
		}
		result, err = c.SetTaskSubscriber(ctx, id, fs.Arg(1), *source, command == "subscribe")
	}
	if err != nil {
		return err
	}
	return printJSON(out, result)
}

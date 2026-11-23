package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"
)

const taskDocRefUsage = "usage: tasks doc-ref add|rm <task> --doc <key>\n       tasks doc-ref list <task>\n\nA doc-ref is a reference from a task to a document it does not own (hk 6125). The document's binding is unchanged; the task page lists references marked 참조."

// taskDocRefCmd serves "tasks doc-ref add|list|rm <task> --doc <key>": the
// task's references to documents it does not own. The doc flag repeats on add
// and rm; list takes none.
func taskDocRefCmd(args []string, out io.Writer) error {
	if len(args) < 3 {
		return errors.New(taskDocRefUsage)
	}
	sub := args[1]
	if sub != "add" && sub != "list" && sub != "rm" {
		return errors.New(taskDocRefUsage)
	}
	fs := flag.NewFlagSet("tasks doc-ref "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var docs stringSliceFlag
	fs.Var(&docs, "doc", "document key to reference (repeatable)")
	ordered, err := splitArgs(args[2:], "doc")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(taskDocRefUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if sub == "list" {
		if len(docs) > 0 {
			return errors.New("tasks doc-ref list takes no --doc")
		}
	} else if len(docs) == 0 {
		return errors.New("tasks doc-ref " + sub + " requires --doc <key>")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch sub {
	case "list":
		refs, err := c.ListTaskDocRefs(ctx, id)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"task_id": id, "doc_refs": refs})
	case "add":
		var changes []any
		for _, doc := range docs {
			change, err := c.AddTaskDocRef(ctx, id, doc)
			if err != nil {
				return err
			}
			changes = append(changes, change)
		}
		return printJSON(out, map[string]any{"changes": changes})
	default:
		var changes []any
		for _, doc := range docs {
			change, err := c.RemoveTaskDocRef(ctx, id, doc)
			if err != nil {
				return err
			}
			changes = append(changes, change)
		}
		return printJSON(out, map[string]any{"changes": changes})
	}
}

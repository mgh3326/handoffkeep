package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskOriginCmd records and looks up where a task came from, by the
// provider's durable id (a Linear issue UUID), not by its display number.
//
//	tasks origin add ID --provider P --namespace NS --external-id UUID [--display-ref R] [--origin-url U] [--title T]
//	tasks origin list ID
//	tasks origin find --provider P --external-id UUID
//
// The origin's URL flag is --origin-url because --url is the hk server on
// every command.
func taskOriginCmd(args []string, out io.Writer) error {
	const usage = "usage: tasks origin add|list|find"
	if len(args) == 0 {
		return errors.New(usage)
	}
	sub := args[0]
	if sub != "add" && sub != "list" && sub != "find" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("tasks origin "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var in store.TaskOriginInput
	valueFlags := []string{}
	if sub != "list" {
		fs.StringVar(&in.Provider, "provider", "", "origin provider: linear, github or sentry")
		fs.StringVar(&in.ExternalID, "external-id", "", "the provider's durable id (a Linear issue UUID)")
		valueFlags = append(valueFlags, "provider", "external-id")
	}
	if sub == "add" {
		fs.StringVar(&in.Namespace, "namespace", "", "the provider's workspace or repository")
		fs.StringVar(&in.DisplayRef, "display-ref", "", "the number a person reads, such as MGH-151")
		fs.StringVar(&in.URL, "origin-url", "", "https URL of the external object (--url is the hk server)")
		fs.StringVar(&in.Title, "title", "", "title at the time of recording")
		valueFlags = append(valueFlags, "namespace", "display-ref", "origin-url", "title")
	}
	ordered, err := splitArgs(args[1:], valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	var id int64
	if sub == "find" {
		if fs.NArg() != 0 {
			return errors.New("tasks origin find takes flags only")
		}
		if in.Provider == "" || in.ExternalID == "" {
			return errors.New("tasks origin find requires --provider and --external-id")
		}
	} else {
		if fs.NArg() != 1 {
			return errors.New("tasks origin " + sub + " requires a task id")
		}
		id, err = strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("task id must be positive")
		}
	}
	if sub == "add" && (in.Provider == "" || in.Namespace == "" || in.ExternalID == "") {
		return errors.New("tasks origin add requires --provider, --namespace and --external-id")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch sub {
	case "add":
		res, err := c.AddTaskOrigin(ctx, id, in)
		if err != nil {
			return err
		}
		return printJSON(out, res)
	case "list":
		xs, err := c.ListTaskOrigins(ctx, id)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"origins": xs})
	default:
		xs, err := c.FindTaskOrigins(ctx, in.Provider, in.ExternalID)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"origins": xs})
	}
}

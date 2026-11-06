package main

import (
	"context"
	"errors"
	"flag"
	"github.com/mgh3326/handoffkeep/internal/store"
	"io"
	"time"
)

const standingUsage = "usage: handoffkeep standing put (--project P | --lane L) --topic K --statement S --source-doc KEY --decided-by WHO --decided-at RFC3339 | standing list [--project P] [--lane L]"

func standingCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "put" && args[0] != "list") {
		return errors.New(standingUsage)
	}
	action := args[0]
	fs := flag.NewFlagSet("standing "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	project := fs.String("project", "", "")
	lane := fs.String("lane", "", "")
	var in store.StandingOrderInput
	var decidedAt string
	if action == "put" {
		fs.StringVar(&in.TopicKey, "topic", "", "")
		fs.StringVar(&in.Statement, "statement", "", "")
		fs.StringVar(&in.SourceDoc, "source-doc", "", "")
		fs.StringVar(&in.DecidedBy, "decided-by", "", "")
		fs.StringVar(&decidedAt, "decided-at", "", "")
	}
	parsed, err := splitArgs(args[1:], "project", "lane", "topic", "statement", "source-doc", "decided-by", "decided-at")
	if err != nil {
		return errors.New(standingUsage)
	}
	if fs.Parse(parsed) != nil || fs.NArg() != 0 {
		return errors.New(standingUsage)
	}
	if action == "put" {
		if (*project == "") == (*lane == "") || in.TopicKey == "" || in.Statement == "" || in.SourceDoc == "" || in.DecidedBy == "" || decidedAt == "" {
			return errors.New(standingUsage)
		}
		in.DecidedAt, err = time.Parse(time.RFC3339, decidedAt)
		if err != nil {
			return errors.New(standingUsage)
		}
		if *project != "" {
			in.ScopeKind = "project"
			in.Scope = *project
		} else {
			in.ScopeKind = "lane"
			in.Scope = *lane
		}
	} else if *project == "" && *lane == "" {
		return errors.New(standingUsage)
	}
	if err = mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if action == "put" {
		result, e := c.PutStandingOrder(ctx, in)
		if e != nil {
			return e
		}
		return printJSON(out, result)
	}
	result, e := c.ListStandingOrders(ctx, *project, *lane)
	if e != nil {
		return e
	}
	return printJSON(out, result)
}

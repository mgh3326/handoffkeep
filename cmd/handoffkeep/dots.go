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

const dotsUsage = "usage: dots request --title TITLE --body BODY [--requester-lane L] [--project P]\n       dots results [--task N] [--issue ID]\n       dots adopt <id> --state accepted|partially_used|rejected|superseded [--ref R] [--superseded-by ID]"

func dotsRequestCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dots request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var in store.DotsRequestInput
	fs.StringVar(&in.Title, "title", "", "request title")
	fs.StringVar(&in.Body, "body", "", "request body")
	fs.StringVar(&in.RequesterLane, "requester-lane", "", "lane to notify")
	fs.StringVar(&in.Project, "project", "", "task project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || in.Title == "" || in.Body == "" {
		return errors.New("dots request requires --title and --body")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task, err := c.CreateDotsRequest(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"task": task})
}

// dotsCmd lists recorded dots answers and records what became of them.
//
//	dots results --task N | --issue ROB-1
//	dots adopt ID --state S [--ref R] [--superseded-by ID]
func dotsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "request" && args[0] != "results" && args[0] != "adopt") {
		return errors.New(dotsUsage)
	}
	if args[0] == "request" {
		return dotsRequestCmd(args[1:], out)
	}
	sub := args[0]
	fs := flag.NewFlagSet("dots "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var (
		taskID int64
		issue  string
		in     store.DotsAdoptionInput
	)
	var valueFlags []string
	if sub == "results" {
		fs.Int64Var(&taskID, "task", 0, "hk task id")
		fs.StringVar(&issue, "issue", "", "Linear issue identifier, such as ROB-1")
		valueFlags = []string{"task", "issue"}
	} else {
		fs.StringVar(&in.State, "state", "", "accepted, partially_used, rejected or superseded")
		fs.StringVar(&in.Ref, "ref", "", "what adopted it: a GitHub PR URL, hk:task/<id> or hk:doc <key>")
		fs.Int64Var(&in.SupersededBy, "superseded-by", 0, "id of the result that replaced it")
		valueFlags = []string{"state", "ref", "superseded-by"}
	}
	ordered, err := splitArgs(args[1:], valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	var id int64
	if sub == "results" {
		if fs.NArg() != 0 || (taskID > 0) == (issue != "") || taskID < 0 {
			return errors.New("dots results takes exactly one of --task N or --issue ID")
		}
	} else {
		if fs.NArg() != 1 {
			return errors.New("dots adopt requires a result id")
		}
		if id, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || id < 1 {
			return errors.New("result id must be positive")
		}
		if in.State == "" {
			return errors.New("dots adopt requires --state")
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if sub == "results" {
		xs, err := c.ListDotsResults(ctx, taskID, issue)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"results": xs})
	}
	res, err := c.SetDotsAdoption(ctx, id, in)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

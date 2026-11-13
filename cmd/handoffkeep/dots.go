package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const dotsUsage = "usage: dots request --title TITLE --body BODY [--type CREATE_MOCKUP|CREATE_ASSET|REVIEW_IMPLEMENTATION] [--requester-lane L] [--project P] [--idempotency-key KEY]\n       dots results [--task N] [--issue ID]\n       dots adopt <id> --state accepted|partially_used|rejected|superseded [--ref R] [--superseded-by ID] [--canva-url U --canva-page P --canva-version V]\n       dots answer <task> --text <answer>\n\nNote: A requester-lane comment alone does NOT resolve an open dots question (explicit beats implicit); use dots answer."

func dotsRequestCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dots request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var in store.DotsRequestInput
	fs.StringVar(&in.Title, "title", "", "request title")
	fs.StringVar(&in.Body, "body", "", "request body")
	fs.StringVar(&in.RequesterLane, "requester-lane", "", "lane to notify")
	fs.StringVar(&in.Project, "project", "", "task project")
	fs.StringVar(&in.IdempotencyKey, "idempotency-key", "", "reuse to retry the same request")
	fs.StringVar(&in.Type, "type", "", "CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION; a typed body needs the policy template with [입력] naming the commit SHA and capture environment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || in.Title == "" || in.Body == "" {
		return errors.New("dots request requires --title and --body")
	}
	if in.RequesterLane == "" {
		in.RequesterLane = os.Getenv("PANEWIRE_LANE")
	}
	if in.RequesterLane == "" {
		return errors.New("dots request requires --requester-lane or PANEWIRE_LANE")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if in.Project == "" {
		owners, err := c.ListProjectOwners(ctx)
		if err != nil {
			return err
		}
		var project string
		matches := 0
		for _, owner := range owners {
			if owner.DesignLane == in.RequesterLane {
				project = owner.Name
				matches++
			}
		}
		if matches == 1 {
			in.Project = project
		}
	}
	task, err := c.CreateDotsRequest(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"task": task})
}

// dotsCmd lists recorded dots answers and records what became of them.
//
//	dots results --task N | --issue ROB-1
//	dots adopt ID --state S [--ref R] [--superseded-by ID] [--canva-url U --canva-page P --canva-version V]
//	dots answer TASK --text ANSWER
func dotsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "request" && args[0] != "results" && args[0] != "adopt" && args[0] != "answer") {
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
		text   string
	)
	var valueFlags []string
	switch sub {
	case "results":
		fs.Int64Var(&taskID, "task", 0, "hk task id")
		fs.StringVar(&issue, "issue", "", "Linear issue identifier, such as ROB-1")
		valueFlags = []string{"task", "issue"}
	case "adopt":
		fs.StringVar(&in.State, "state", "", "accepted, partially_used, rejected or superseded")
		fs.StringVar(&in.Ref, "ref", "", "what adopted it: a GitHub PR URL, hk:task/<id> or hk:doc <key>")
		fs.Int64Var(&in.SupersededBy, "superseded-by", 0, "id of the result that replaced it")
		fs.StringVar(&in.CanvaURL, "canva-url", "", "Canva design link, an https canva.com design URL; with --canva-page and --canva-version, for accepted or partially_used")
		fs.StringVar(&in.CanvaPage, "canva-page", "", "Canva page the adopted design is on")
		fs.StringVar(&in.CanvaVersion, "canva-version", "", "Canva version of that page")
		valueFlags = []string{"state", "ref", "superseded-by", "canva-url", "canva-page", "canva-version"}
	case "answer":
		fs.StringVar(&text, "text", "", "answer text")
		valueFlags = []string{"text"}
	}
	ordered, err := splitArgs(args[1:], valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	var id int64
	switch sub {
	case "results":
		if fs.NArg() != 0 || (taskID > 0) == (issue != "") || taskID < 0 {
			return errors.New("dots results takes exactly one of --task N or --issue ID")
		}
	case "adopt":
		if fs.NArg() != 1 {
			return errors.New("dots adopt requires a result id")
		}
		if id, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || id < 1 {
			return errors.New("result id must be positive")
		}
		if in.State == "" {
			return errors.New("dots adopt requires --state")
		}
	case "answer":
		if fs.NArg() != 1 {
			return errors.New("dots answer requires a task id")
		}
		if id, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || id < 1 {
			return errors.New("task id must be positive")
		}
		if text == "" {
			return errors.New("dots answer requires --text")
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch sub {
	case "results":
		xs, err := c.ListDotsResults(ctx, taskID, issue)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"results": xs})
	case "adopt":
		res, err := c.SetDotsAdoption(ctx, id, in)
		if err != nil {
			return err
		}
		return printJSON(out, res)
	case "answer":
		res, err := c.AnswerDots(ctx, id, text)
		if err != nil {
			return err
		}
		return printJSON(out, res)
	default:
		return errors.New(dotsUsage)
	}
}

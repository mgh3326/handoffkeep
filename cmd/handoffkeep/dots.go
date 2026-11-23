package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const dotsUsage = "usage: dots request --title TITLE --body BODY [--type CREATE_MOCKUP|CREATE_ASSET|REVIEW_IMPLEMENTATION] [--requester-lane L] [--project P] [--idempotency-key KEY] [--option 'A|label' ...] [--option-attachment 'A|sha256[|caption]' ...] [--doc KEY ...]\n       dots results [--task N] [--issue ID]\n       dots reads [--since RFC3339] [--limit N]\n       dots adopt <id> --state accepted|partially_used|rejected|superseded [--ref R] [--superseded-by ID] [--canva-url U --canva-page P --canva-version V]\n       dots answer <task> --text <answer> [--requester-lane L] [--question-id N]\n       dots cancel <task> --reason REASON [--requester-lane L]\n\nNote: dots answer answers an open decision request, or a plain dots QUESTION with the request's requester lane (--requester-lane defaults to PANEWIRE_LANE); a question is answered once. With several questions open, --question-id names the reply (its id in dots_inbox).\nNote: A requester-lane comment alone does NOT resolve an open dots question (explicit beats implicit); use dots answer.\nNote: A requester-lane comment alone does NOT cancel a request; use dots cancel, which only the request's requester lane may run.\nNote: --option declares that the request ends in the operator's choice; its RESULT opens the decision."

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
	fs.StringVar(&in.Type, "type", "", "CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION; a typed body needs the policy template whose [입력] names the commit SHA and capture environment, or for CREATE_ASSET the source material (an hk attachment sha256, an hk:doc key, or a 원본: description)")
	var optionValues, attachmentValues, docValues stringSliceFlag
	fs.Var(&optionValues, "option", "option A|label the request ends in the operator's choice between (repeatable, 1-6); dots names its recommendation on RESULT")
	fs.Var(&attachmentValues, "option-attachment", "option A|sha256[|caption]: a picture or video shown with that option (repeatable, at most 8 per option)")
	fs.Var(&docValues, "doc", "document key the request cites, recorded as a task reference (repeatable, at most 20); the document must exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in.DocKeys = docValues
	if fs.NArg() != 0 || in.Title == "" || in.Body == "" {
		return errors.New("dots request requires --title and --body")
	}
	if len(optionValues) > 0 {
		options := store.DecisionOptions{}
		for _, value := range optionValues {
			key, label, ok := strings.Cut(value, "|")
			if !ok {
				return errors.New("--option must be A|label")
			}
			options.Options = append(options.Options, store.DecisionOption{Key: strings.TrimSpace(key), Label: strings.TrimSpace(label)})
		}
		if err := attachOptionMedia(&options, attachmentValues); err != nil {
			return err
		}
		in.Options = options.Options
	} else if len(attachmentValues) > 0 {
		return errors.New("--option-attachment names no --option")
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
//	dots reads [--since RFC3339] [--limit N]
//	dots adopt ID --state S [--ref R] [--superseded-by ID] [--canva-url U --canva-page P --canva-version V]
//	dots answer TASK --text ANSWER [--requester-lane L]
//	dots cancel TASK --reason REASON [--requester-lane L]
func dotsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "request" && args[0] != "results" && args[0] != "reads" && args[0] != "adopt" && args[0] != "answer" && args[0] != "cancel") {
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
		taskID     int64
		issue      string
		in         store.DotsAdoptionInput
		answerIn   store.DotsAnswerInput
		cancelIn   store.DotsCancelInput
		readsSince string
		readsLimit int
	)
	var valueFlags []string
	switch sub {
	case "results":
		fs.Int64Var(&taskID, "task", 0, "hk task id")
		fs.StringVar(&issue, "issue", "", "Linear issue identifier, such as ROB-1")
		valueFlags = []string{"task", "issue"}
	case "reads":
		fs.StringVar(&readsSince, "since", "", "RFC3339; only reads at or after this time")
		fs.IntVar(&readsLimit, "limit", 0, "at most N reads; the server default applies when unset")
		valueFlags = []string{"since", "limit"}
	case "adopt":
		fs.StringVar(&in.State, "state", "", "accepted, partially_used, rejected or superseded")
		fs.StringVar(&in.Ref, "ref", "", "what adopted it: a GitHub PR URL, hk:task/<id> or hk:doc <key>")
		fs.Int64Var(&in.SupersededBy, "superseded-by", 0, "id of the result that replaced it")
		fs.StringVar(&in.CanvaURL, "canva-url", "", "Canva design link, an https canva.com design URL; with --canva-page and --canva-version, for accepted or partially_used")
		fs.StringVar(&in.CanvaPage, "canva-page", "", "Canva page the adopted design is on")
		fs.StringVar(&in.CanvaVersion, "canva-version", "", "Canva version of that page")
		valueFlags = []string{"state", "ref", "superseded-by", "canva-url", "canva-page", "canva-version"}
	case "answer":
		fs.StringVar(&answerIn.Text, "text", "", "answer text")
		fs.StringVar(&answerIn.RequesterLane, "requester-lane", "", "the request's requester lane when answering a plain QUESTION; defaults to PANEWIRE_LANE")
		fs.Int64Var(&answerIn.QuestionID, "question-id", 0, "the dots QUESTION reply id to answer when more than one is open (the reply's id in dots_inbox)")
		valueFlags = []string{"text", "requester-lane", "question-id"}
	case "cancel":
		fs.StringVar(&cancelIn.Reason, "reason", "", "why the request is cancelled (recorded on the task)")
		fs.StringVar(&cancelIn.RequesterLane, "requester-lane", "", "lane that filed the request; defaults to PANEWIRE_LANE, and must be that request's requester lane")
		valueFlags = []string{"reason", "requester-lane"}
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
	case "reads":
		if fs.NArg() != 0 {
			return errors.New("dots reads takes no arguments")
		}
		if readsLimit < 0 {
			return errors.New("dots reads --limit must be positive")
		}
		if readsSince != "" {
			if _, err := time.Parse(time.RFC3339, readsSince); err != nil {
				return errors.New("dots reads --since must be an RFC3339 time")
			}
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
		if answerIn.Text == "" {
			return errors.New("dots answer requires --text")
		}
	case "cancel":
		if fs.NArg() != 1 {
			return errors.New("dots cancel requires a task id")
		}
		if id, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || id < 1 {
			return errors.New("task id must be positive")
		}
		if strings.TrimSpace(cancelIn.Reason) == "" {
			return errors.New("dots cancel requires --reason")
		}
		if cancelIn.RequesterLane == "" {
			cancelIn.RequesterLane = os.Getenv("PANEWIRE_LANE")
		}
		if cancelIn.RequesterLane == "" {
			return errors.New("dots cancel requires --requester-lane or PANEWIRE_LANE")
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
	case "reads":
		var since time.Time
		if readsSince != "" {
			since, _ = time.Parse(time.RFC3339, readsSince)
		}
		xs, err := c.ListDotsInboxReads(ctx, since, readsLimit)
		if err != nil {
			return err
		}
		answer := map[string]any{"reads": xs}
		if len(xs) > 0 {
			answer["last_read_at"] = xs[0].ReadAt
			if len(xs) > 1 {
				answer["gap_seconds"] = xs[0].ReadAt.Sub(xs[1].ReadAt).Seconds()
			}
		}
		return printJSON(out, answer)
	case "adopt":
		res, err := c.SetDotsAdoption(ctx, id, in)
		if err != nil {
			return err
		}
		return printJSON(out, res)
	case "answer":
		if answerIn.RequesterLane == "" {
			answerIn.RequesterLane = os.Getenv("PANEWIRE_LANE")
		}
		res, err := c.AnswerDots(ctx, id, answerIn.Text, answerIn.RequesterLane, answerIn.QuestionID)
		if err != nil {
			return err
		}
		return printJSON(out, res)
	case "cancel":
		task, err := c.CancelDotsRequest(ctx, id, cancelIn)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"task": task})
	default:
		return errors.New(dotsUsage)
	}
}

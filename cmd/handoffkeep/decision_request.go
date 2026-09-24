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

const decisionRequestUsage = "usage: tasks decision-request <id> --question <text> --option 'A|label' [--option ...] [--recommended A] [--reason <text>] --default-action <text> [--default-option B] [--default-trigger <text>] [--due <RFC3339>] [--doc <key>] [--supersedes dr-<id>-<rev>] [--block] [--no-free-answer]"

const decisionResolveUsage = "usage: tasks decision-resolve <id> --request dr-<id>-<rev> --kind answered|default_applied|withdrawn [--option A] [--text <text>] [--receipt <ref>] [--responder <who>]"

// splitSubcommandArgs lets flags and the positional id come in any order.
func splitSubcommandArgs(rest []string, valueFlags map[string]bool) ([]string, error) {
	flags, positional := []string{}, []string{}
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
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, rest[i])
		}
	}
	return append(flags, positional...), nil
}

func parseSubcommandTaskID(fs *flag.FlagSet, usage string) (int64, error) {
	if fs.NArg() != 1 {
		return 0, errors.New(usage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("task id must be positive")
	}
	return id, nil
}

// notRecorded wraps every failure of a request write. A6: a producer that
// sees this must not tell the operator the request is visible in the
// console — nothing was recorded.
func notRecorded(err error) error {
	return fmt.Errorf("decision request NOT recorded: %v — do not notify that it is visible in the console", err)
}

// decisionRequestCmd records a structured decision request (#618). The
// request is recorded in hk first; the printed request_id is what the pane
// notification carries.
func decisionRequestCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks decision-request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	question := fs.String("question", "", "the question put to the operator")
	var optionValues refFlags
	fs.Var(&optionValues, "option", "decision option A|label (repeatable, 1-6, label at most 120 bytes)")
	recommended := fs.String("recommended", "", "recommended option key")
	reason := fs.String("reason", "", "why the recommendation")
	defaultAction := fs.String("default-action", "", "what happens if nobody answers (\"자동 적용 없음\" when nothing is applied)")
	defaultOption := fs.String("default-option", "", "option key the default action corresponds to, when it is one")
	defaultTrigger := fs.String("default-trigger", "", "when/how the default is applied")
	due := fs.String("due", "", "response deadline, RFC3339 with a timezone offset")
	doc := fs.String("doc", "", "hk document holding long outcome text and evidence")
	supersedes := fs.String("supersedes", "", "the open request this one replaces")
	block := fs.Bool("block", false, "also move the task to needs_decision")
	noFreeAnswer := fs.Bool("no-free-answer", false, "disallow a free-form answer")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--question": true, "--option": true, "--recommended": true, "--reason": true, "--default-action": true, "--default-option": true, "--default-trigger": true, "--due": true, "--doc": true, "--supersedes": true}
	parseArgs, err := splitSubcommandArgs(args[1:], valueFlags)
	if err != nil {
		return err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	id, err := parseSubcommandTaskID(fs, decisionRequestUsage)
	if err != nil {
		return err
	}
	input := store.DecisionRequestInput{
		Question:       strings.TrimSpace(*question),
		Reason:         strings.TrimSpace(*reason),
		DefaultAction:  strings.TrimSpace(*defaultAction),
		DefaultOption:  strings.TrimSpace(*defaultOption),
		DefaultTrigger: strings.TrimSpace(*defaultTrigger),
		Doc:            strings.TrimSpace(*doc),
		Supersedes:     strings.TrimSpace(*supersedes),
		Block:          *block,
	}
	if len(optionValues) == 0 {
		return notRecorded(errors.New("at least one --option is required"))
	}
	input.Options, err = parseTaskDecisionOptions(optionValues, *recommended, *noFreeAnswer)
	if err != nil {
		return notRecorded(err)
	}
	if strings.TrimSpace(*due) != "" {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*due))
		if err != nil {
			return notRecorded(errors.New("--due must be RFC3339 with a timezone offset, e.g. 2026-09-25T18:00:00+09:00"))
		}
		input.DueAt = &at
	}
	if err := store.ValidateDecisionRequestInput(input); err != nil {
		return notRecorded(err)
	}
	if err := mustClient(c); err != nil {
		return notRecorded(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.RecordDecisionRequest(ctx, id, input)
	if err != nil {
		return notRecorded(err)
	}
	return printJSON(out, decisionRequestOutput(result))
}

// decisionRequestOutput is the CLI's answer after the record exists. notify
// is the line to put in the pane message: it carries the request_id.
func decisionRequestOutput(result store.DecisionRequestResult) map[string]any {
	request := result.Request
	notify := fmt.Sprintf("[decision-request] #%d %s (r%d) recorded in hk — console /ui/queue?task=%d", result.Task.ID, request.ID, request.Revision, result.Task.ID)
	if result.Duplicate {
		notify = fmt.Sprintf("[decision-request] #%d %s (r%d, status %s) already recorded — re-send with the same request_id", result.Task.ID, request.ID, request.Revision, request.Status)
	}
	return map[string]any{
		"recorded":   true,
		"duplicate":  result.Duplicate,
		"task_id":    result.Task.ID,
		"state":      result.Task.State,
		"request_id": request.ID,
		"revision":   request.Revision,
		"status":     request.Status,
		"notify":     notify,
		"request":    request,
	}
}

// decisionResolveCmd records how a request closed. It never changes the
// task state; default_applied needs the receipt of the application.
func decisionResolveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks decision-resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	requestID := fs.String("request", "", "request id being resolved (dr-<id>-<rev>)")
	kind := fs.String("kind", "", "answered, default_applied or withdrawn")
	option := fs.String("option", "", "chosen option key")
	text := fs.String("text", "", "answer text, application note, or withdrawal reason")
	receipt := fs.String("receipt", "", "evidence the default was applied (event id, PR, or document)")
	responder := fs.String("responder", "", "who answered, when recording an answer given elsewhere")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--request": true, "--kind": true, "--option": true, "--text": true, "--receipt": true, "--responder": true}
	parseArgs, err := splitSubcommandArgs(args[1:], valueFlags)
	if err != nil {
		return err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	id, err := parseSubcommandTaskID(fs, decisionResolveUsage)
	if err != nil {
		return err
	}
	input := store.DecisionResolveInput{
		RequestID: strings.TrimSpace(*requestID),
		Kind:      strings.TrimSpace(*kind),
		Option:    strings.TrimSpace(*option),
		Text:      strings.TrimSpace(*text),
		Receipt:   strings.TrimSpace(*receipt),
		Responder: strings.TrimSpace(*responder),
	}
	if err := store.ValidateDecisionResolveInput(input); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.ResolveDecisionRequest(ctx, id, input)
	if err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	return printJSON(out, map[string]any{
		"recorded":   true,
		"duplicate":  result.Duplicate,
		"task_id":    result.Task.ID,
		"state":      result.Task.State,
		"request_id": result.Request.ID,
		"status":     result.Request.Status,
		"request":    result.Request,
	})
}

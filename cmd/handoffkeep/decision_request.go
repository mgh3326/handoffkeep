package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const decisionRequestUsage = "usage: tasks decision-request <id> --question <text> --option 'A|label' [--option ...] [--recommended A] [--reason <text>] --default-action <text> [--default-option B] [--default-trigger <text>] [--due <RFC3339>] [--doc <key>] [--ref <text> ...] [--supersedes dr-<id>-<rev>] [--block] [--no-free-answer] [--human-only] [--answer-policy operator_direct|operator_relay_allowed]"

const decisionAskUsage = "usage: decisions ask <task-id> --question <text> --option 'A|label' [--option ...] [--recommended A] [--reason <text>] --default-action <text> [--default-option B] [--default-trigger <text>] [--due <RFC3339>] [--doc <key>] [--ref <text> ...] [--supersedes dr-<id>-<rev>] [--block] [--no-free-answer] [--human-only] [--answer-policy operator_direct|operator_relay_allowed] [--linear]"

const decisionResolveRequestUsage = "usage: decisions resolve request <dr-id> --channel cli|session [--option A] [--text <text>] [--quote '<operator words>'] [--event-key <key>]"

const decisionResolveUsage = "usage: tasks decision-resolve <id> --request dr-<id>-<rev> --kind answered|default_applied|withdrawn [--option A] [--text <text>] [--receipt <ref>] [--responder <who>] [--event-key <key>]"

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

// exitWriteUnknown is the exit status when a write's outcome is unknown.
const exitWriteUnknown = 4

// serverRefused reports whether err is a refusal the server sent before
// writing anything: auth (401/403), unknown task (404), a documented
// conflict (409), or a named validation error (400). "invalid_context" is the
// API's catch-all for unexpected store errors — a failed commit among them —
// so it does not prove the write was refused.
func serverRefused(err error) bool {
	var httpErr *remote.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.Status {
	case 401, 403, 404, 409:
		return true
	case 400:
		return httpErr.Code != "" && httpErr.Code != "invalid_context"
	case 503:
		// The ask route answers this before it writes anything.
		return httpErr.Code == "linear_decisions_disabled"
	}
	return false
}

// writeFailed classifies an error returned by a decision write. Only a known
// refusal is "NOT recorded". A transport error, timeout, undecodable reply or
// server error after the POST left the process may come after the server
// committed — reporting it as not recorded would invite a second, different
// request; reporting it as recorded would be a guess. It is UNKNOWN: check
// the task, or re-send the identical command, which returns the recorded
// request as a duplicate.
func writeFailed(what string, id int64, err error) error {
	if serverRefused(err) {
		return fmt.Errorf("%s NOT recorded: %v — do not notify that it is visible in the console", what, err)
	}
	return exitCodeError{code: exitWriteUnknown, err: fmt.Errorf("%s outcome UNKNOWN: %v — the server may have recorded it; check `handoffkeep tasks show %d` (refs.decision_request) or re-send the identical command (a recorded request comes back as duplicate); do not notify until confirmed", what, err, id)}
}

// notRecorded wraps every failure of a request write. A6: a producer that
// sees this must not tell the operator the request is visible in the
// console — nothing was recorded.
func notRecorded(err error) error {
	return fmt.Errorf("decision request NOT recorded: %v — do not notify that it is visible in the console", err)
}

type decisionTranslationFlags struct {
	question *string
	options  refFlags
}

func addDecisionTranslationFlags(fs *flag.FlagSet) *decisionTranslationFlags {
	flags := &decisionTranslationFlags{question: fs.String("question-ko", "", "Korean question")}
	fs.Var(&flags.options, "option-ko", "Korean option A|label (repeatable)")
	return flags
}

func putDecisionTranslations(ctx context.Context, c *remote.Client, request store.DecisionRequest, input store.DecisionRequestInput, flags *decisionTranslationFlags) error {
	rows := []store.Translation{}
	if *flags.question != "" {
		rows = append(rows, store.Translation{Entity: "decision_request", EntityID: request.ID, Field: "question", Lang: "ko", SourceSHA256: store.SourceSHA256(request.Question), Text: *flags.question, Translator: "cli"})
	}
	if len(flags.options) > 0 {
		translated, err := parseTaskDecisionOptions(flags.options, "", !input.Options.AllowFree)
		if err != nil {
			return err
		}
		if len(translated.Options) != len(input.Options.Options) {
			return errors.New("--option-ko must supply one translation per option")
		}
		for i := range translated.Options {
			if translated.Options[i].Key != input.Options.Options[i].Key {
				return errors.New("--option-ko keys must match --option order")
			}
			translated.Options[i].Recommended = input.Options.Options[i].Recommended
		}
		source, err := json.Marshal(input.Options)
		if err != nil {
			return err
		}
		target, err := json.Marshal(translated)
		if err != nil {
			return err
		}
		rows = append(rows, store.Translation{Entity: "decision_request", EntityID: request.ID, Field: "options", Lang: "ko", SourceSHA256: store.SourceSHA256(string(source)), Text: string(target), Translator: "cli"})
	}
	if len(rows) > 0 {
		if _, err := c.PutTranslations(ctx, rows); err != nil {
			return fmt.Errorf("decision %s recorded, but translation failed: %w", request.ID, err)
		}
	}
	return nil
}

// decisionRequestFlags parses the flags a decision request is made of into
// fs, which the caller may have given flags of its own. The caller says "NOT
// recorded" on any error: nothing has left the process yet.
func decisionRequestFlags(fs *flag.FlagSet, usage string, args []string) (*remote.Client, int64, store.DecisionRequestInput, error) {
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	question := fs.String("question", "", "the question put to the operator")
	var optionValues, refValues refFlags
	fs.Var(&optionValues, "option", "decision option A|label (repeatable, 1-6, label at most 120 bytes)")
	fs.Var(&refValues, "ref", "short pointer the question depends on (repeatable, counts toward ver)")
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
	humanOnly := fs.Bool("human-only", false, "reserve the request for the human operator; the assistant path refuses it")
	answerPolicy := fs.String("answer-policy", "", "operator_direct (a relayed answer waits for the operator) or operator_relay_allowed (default)")
	parseArgs, err := splitArgs(args, "question", "question-ko", "option", "option-ko", "ref", "recommended", "reason", "default-action", "default-option", "default-trigger", "due", "doc", "supersedes", "answer-policy")
	if err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	id, err := parseSubcommandTaskID(fs, usage)
	if err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	input := store.DecisionRequestInput{
		Question:       strings.TrimSpace(*question),
		Reason:         strings.TrimSpace(*reason),
		DefaultAction:  strings.TrimSpace(*defaultAction),
		DefaultOption:  strings.TrimSpace(*defaultOption),
		DefaultTrigger: strings.TrimSpace(*defaultTrigger),
		Doc:            strings.TrimSpace(*doc),
		Refs:           refValues,
		Supersedes:     strings.TrimSpace(*supersedes),
		Block:          *block,
		HumanOnly:      *humanOnly,
		AnswerPolicy:   strings.TrimSpace(*answerPolicy),
	}
	if len(optionValues) == 0 {
		return nil, 0, store.DecisionRequestInput{}, errors.New("at least one --option is required")
	}
	input.Options, err = parseTaskDecisionOptions(optionValues, *recommended, *noFreeAnswer)
	if err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	if strings.TrimSpace(*due) != "" {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*due))
		if err != nil {
			return nil, 0, store.DecisionRequestInput{}, errors.New("--due must be RFC3339 with a timezone offset, e.g. 2026-09-25T18:00:00+09:00")
		}
		input.DueAt = &at
	}
	if err := store.CheckDecisionRequestInput(input); err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	if err := mustClient(c); err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	return c, id, input, nil
}

// decisionRequestCmd records a structured decision request (#618). The
// request is recorded in hk first; the printed request_id is what the pane
// notification carries.
func decisionRequestCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks decision-request", flag.ContinueOnError)
	ko := addDecisionTranslationFlags(fs)
	c, id, input, err := decisionRequestFlags(fs, decisionRequestUsage, args[1:])
	if err != nil {
		return notRecorded(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.RecordDecisionRequest(ctx, id, input)
	if err != nil {
		return writeFailed("decision request", id, err)
	}
	output := decisionRequestOutput(result)
	if err := putDecisionTranslations(ctx, c, result.Request, input, ko); err != nil {
		output["translation_error"] = "translation_write_failed"
	}
	return printJSON(out, output)
}

// decisionAskCmd records a decision request for the operator. Without
// --linear it is the record-only route and opens no Linear issue; with
// --linear the server also opens the request's Linear issue, so it gets a
// longer budget than a plain record. Re-sending the same command after any
// failure creates nothing twice.
func decisionAskCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions ask", flag.ContinueOnError)
	linear := fs.Bool("linear", false, "also open the request's Linear issue")
	ko := addDecisionTranslationFlags(fs)
	c, id, input, err := decisionRequestFlags(fs, decisionAskUsage, args)
	if err != nil {
		return notRecorded(err)
	}
	var result store.DecisionAskResult
	if *linear {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		result, err = c.AskDecision(ctx, id, input)
		if err != nil {
			return writeFailed("decision ask", id, err)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		recorded, err := c.RecordDecisionRequest(ctx, id, input)
		if err != nil {
			return writeFailed("decision ask", id, err)
		}
		var options store.DecisionOptions
		if recorded.Task.Refs.DecisionOptions != nil {
			options = *recorded.Task.Refs.DecisionOptions
		}
		request := recorded.Request
		result = store.DecisionAskResult{
			Task: recorded.Task, Request: request, Duplicate: recorded.Duplicate,
			DecisionRef: store.DecisionRef(request.ID), Ver: store.DecisionRequestVer(request.Question, options, request.Doc, request.Refs),
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	translationErr := putDecisionTranslations(ctx, c, result.Request, input, ko)
	output := map[string]any{
		"recorded":      true,
		"duplicate":     result.Duplicate,
		"issue_created": result.IssueCreated,
		"task_id":       result.Task.ID,
		"request_id":    result.Request.ID,
		"decision_ref":  result.DecisionRef,
		"ver":           result.Ver,
		"status":        result.Request.Status,
	}
	if result.Issue != nil {
		output["issue"] = result.Issue
	}
	if translationErr != nil {
		output["translation_error"] = "translation_write_failed"
	}
	return printJSON(out, output)
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
func decisionResolveCmd(args []string, out, errout io.Writer) error {
	fs := flag.NewFlagSet("tasks decision-resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	requestID := fs.String("request", "", "request id being resolved (dr-<id>-<rev>)")
	kind := fs.String("kind", "", "answered, default_applied or withdrawn")
	option := fs.String("option", "", "chosen option key")
	text := fs.String("text", "", "answer text, application note, or withdrawal reason")
	receipt := fs.String("receipt", "", "evidence the default was applied (event id, PR, or document)")
	responder := fs.String("responder", "", "who answered, when recording an answer given elsewhere")
	eventKey := fs.String("event-key", "", "delivery key: re-sending the same key and answer replays the first result")
	parseArgs, err := splitArgs(args[1:], "request", "kind", "option", "text", "receipt", "responder", "event-key")
	if err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if err := fs.Parse(parseArgs); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	id, err := parseSubcommandTaskID(fs, decisionResolveUsage)
	if err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	input := store.DecisionResolveInput{
		RequestID: strings.TrimSpace(*requestID),
		Kind:      strings.TrimSpace(*kind),
		Option:    strings.TrimSpace(*option),
		Text:      strings.TrimSpace(*text),
		Receipt:   strings.TrimSpace(*receipt),
		Responder: strings.TrimSpace(*responder),
		EventKey:  strings.TrimSpace(*eventKey),
	}
	if err := store.CheckDecisionResolveInput(input); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if err := mustClient(c); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.ResolveDecisionRequest(ctx, id, input)
	if err != nil {
		var httpErr *remote.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == "decision_request_stale" {
			// A legacy decision task (decision_options from a
			// --question/--option transition) holds no structured dr-
			// request, so nothing went stale — name the working route. A
			// server that predates the awaiting_decision flag reports the
			// old needs_decision state instead.
			if task, found, readErr := c.GetTask(ctx, id); readErr == nil && found &&
				(task.AwaitingDecision || task.State == "needs_decision") && task.Refs.DecisionRequest == nil {
				if task.Refs.Disposition != nil {
					fmt.Fprintf(errout, "task %d is a disposition item waiting on a decision; only the operator's web answer resolves it (decisions resolve is refused with disposition_operator_only)\n", id)
				} else {
					fmt.Fprintf(errout, "task %d is waiting on a decision with no structured decision request (decision_request_stale is misleading: nothing went stale) — resolve it with: handoffkeep decisions resolve task %d --by LANE --answer TEXT --no-inject\n", id, id)
				}
			}
		}
		return writeFailed("decision resolution", id, err)
	}
	return printJSON(out, decisionResolveOutput(result))
}

// decisionResolveOutput is the printed answer of a resolve. An answer that
// must wait for the operator gets 202 pending_confirmation: the request stays
// open and nothing failed, so the command prints the result and exits 0.
func decisionResolveOutput(result store.DecisionRequestResult) map[string]any {
	printed := map[string]any{
		"recorded":   result.Result != store.DecisionResultPendingConfirmation,
		"duplicate":  result.Duplicate,
		"task_id":    result.Task.ID,
		"state":      result.Task.State,
		"request_id": result.Request.ID,
		"status":     result.Request.Status,
		"request":    result.Request,
	}
	if result.Result != "" {
		printed["result"] = result.Result
	}
	if result.ResolutionID != 0 {
		printed["resolution_id"] = result.ResolutionID
	}
	return printed
}

// decisionResolveRequestCmd answers a dr- request through ResolveDecision.
// --channel session records the operator's words from a session: with
// --quote it is the operator's relayed decision bound to this request id,
// without it the session's own judgement. --channel cli is today's
// tasks decision-resolve answer and takes no quote.
func decisionResolveRequestCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions resolve request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	channel := fs.String("channel", "", "cli or session")
	option := fs.String("option", "", "chosen option key")
	text := fs.String("text", "", "answer text")
	quote := fs.String("quote", "", "the operator's verbatim words (session only)")
	eventKey := fs.String("event-key", "", "delivery key: re-sending the same key and answer replays the first result")
	parseArgs, err := splitArgs(args, "channel", "option", "text", "quote", "event-key")
	if err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if err := fs.Parse(parseArgs); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if fs.NArg() != 1 {
		return errors.New(decisionResolveRequestUsage)
	}
	requestID := strings.TrimSpace(fs.Arg(0))
	id, _, ok := store.ParseDecisionRequestID(requestID)
	if !ok {
		return errors.New("request id must be dr-<task>-<revision>\n" + decisionResolveRequestUsage)
	}
	in := remote.DecisionChannelResolveInput{
		DecisionResolveInput: store.DecisionResolveInput{
			RequestID: requestID, Kind: store.DecisionRequestAnswered,
			Option: strings.TrimSpace(*option), Text: strings.TrimSpace(*text), EventKey: strings.TrimSpace(*eventKey),
		},
		Channel: *channel,
	}
	switch *channel {
	case store.DecisionChannelCLI:
		if *quote != "" {
			return errors.New("--quote needs --channel session\n" + decisionResolveRequestUsage)
		}
	case store.DecisionChannelSession:
		in.Provenance = store.DecisionProvenanceAssistantJudgement
		if *quote != "" {
			in.Provenance, in.OperatorQuote, in.DecisionRef = store.DecisionProvenanceOperatorDecided, *quote, requestID
		}
	default:
		return errors.New("--channel must be cli or session\n" + decisionResolveRequestUsage)
	}
	if err := store.CheckDecisionResolveInput(in.DecisionResolveInput); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	if err := mustClient(c); err != nil {
		return fmt.Errorf("decision resolution NOT recorded: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.ResolveDecisionChannel(ctx, id, in)
	if err != nil {
		if remote.ErrorCode(err) == "decision_request_human_only" {
			return fmt.Errorf("decision resolution NOT recorded: the request needs the operator's own answer; pass the operator's words with --quote: %v", err)
		}
		return writeFailed("decision resolution", id, err)
	}
	return printJSON(out, decisionResolveOutput(result))
}

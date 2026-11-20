package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const decisionRequestUsage = "usage: tasks decision-request <id> --question <text> --option 'A|label' [--option ...] [--recommended A] [--reason <text>] [--recommendation-reason <text>] [--unverified <text> ...] [--one-fix <text>] [--option-attachment 'A|sha256[|caption]' ...] --default-action <text> [--default-option B] [--default-trigger <text>] [--due <RFC3339>] [--doc <key>] [--ref <text> ...] [--grounds <text> ...] [--lane <lane>] [--supersedes dr-<id>-<rev>] [--block] [--no-free-answer] [--human-only] [--answer-policy operator_direct|operator_relay_allowed]"

const decisionAskUsage = "usage: decisions ask <task-id> | decisions ask --new-task --project <project> --lane <lane> --question <text> --option 'A|label' [--option ...] [--recommended A] [--reason <text>] [--recommendation-reason <text>] [--unverified <text> ...] [--one-fix <text>] [--option-attachment 'A|sha256[|caption]' ...] --default-action <text> [--default-option B] [--default-trigger <text>] [--due <RFC3339>] [--doc <key>] [--ref <text> ...] [--grounds <text> ...] [--lane <lane>] [--supersedes dr-<id>-<rev>] [--block] [--no-free-answer] [--human-only] [--answer-policy operator_direct|operator_relay_allowed]"

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

func decisionTranslations(input store.DecisionRequestInput, flags *decisionTranslationFlags) ([]store.Translation, error) {
	rows := []store.Translation{}
	if *flags.question != "" {
		rows = append(rows, store.Translation{Entity: "decision_request", Field: "question", Lang: "ko", Text: *flags.question, Translator: "cli"})
	}
	if len(flags.options) > 0 {
		translated, err := parseTaskDecisionOptions(flags.options, "", !input.Options.AllowFree)
		if err != nil {
			return nil, err
		}
		if len(translated.Options) != len(input.Options.Options) {
			return nil, errors.New("--option-ko must supply one translation per option")
		}
		for i := range translated.Options {
			if translated.Options[i].Key != input.Options.Options[i].Key {
				return nil, errors.New("--option-ko keys must match --option order")
			}
			translated.Options[i].Recommended = input.Options.Options[i].Recommended
		}
		source, err := json.Marshal(input.Options)
		if err != nil {
			return nil, err
		}
		target, err := json.Marshal(translated)
		if err != nil {
			return nil, err
		}
		rows = append(rows, store.Translation{Entity: "decision_request", Field: "options", Lang: "ko", SourceSHA256: store.SourceSHA256(string(source)), Text: string(target), Translator: "cli"})
	}
	return rows, nil
}

func putDecisionTranslations(ctx context.Context, c *remote.Client, request store.DecisionRequest, rows []store.Translation) error {
	for i := range rows {
		rows[i].EntityID = request.ID
		if rows[i].Field == "question" {
			rows[i].SourceSHA256 = store.SourceSHA256(request.Question)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := c.PutTranslations(ctx, rows)
	return err
}

// decisionRequestFlags parses the flags a decision request is made of into
// fs, which the caller may have given flags of its own. The caller says "NOT
// recorded" on any error: nothing has left the process yet.
func decisionRequestFlags(fs *flag.FlagSet, usage string, args []string) (*remote.Client, int64, store.DecisionRequestInput, error) {
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	question := fs.String("question", "", "the question put to the operator")
	var optionValues, refValues, unverifiedValues, attachmentValues refFlags
	fs.Var(&optionValues, "option", "decision option A|label (repeatable, 1-6, label at most 120 bytes)")
	fs.Var(&refValues, "ref", "short pointer the question depends on (repeatable, counts toward ver)")
	fs.Var(&unverifiedValues, "unverified", "one thing the requester could not check (repeatable, at most 10)")
	fs.Var(&attachmentValues, "option-attachment", "option A|sha256[|caption]: a picture or video shown with that option (repeatable, at most 8 per option)")
	recommended := fs.String("recommended", "", "recommended option key")
	reason := fs.String("reason", "", "why the recommendation")
	recommendationReason := fs.String("recommendation-reason", "", "the longer account of the recommendation")
	oneFix := fs.String("one-fix", "", "the requester's one suggested fix")
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
	var groundValues refFlags
	fs.Var(&groundValues, "grounds", "a fact the recommendation rests on, one line (repeatable, at most 10)")
	requestLane := fs.String("lane", "", "the lane that asks; its answer notice also goes there when it differs from the task's lane")
	parseArgs, err := splitArgs(args, "question", "question-ko", "option", "option-ko", "option-attachment", "ref", "grounds", "lane", "project", "recommended", "reason", "recommendation-reason", "unverified", "one-fix", "default-action", "default-option", "default-trigger", "due", "doc", "supersedes", "answer-policy")
	if err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	var id int64
	if f := fs.Lookup("new-task"); f != nil && f.Value.String() == "true" {
		// decisions ask --new-task files its task itself: no task id.
		if fs.NArg() != 0 {
			return nil, 0, store.DecisionRequestInput{}, errors.New("--new-task takes no task id: " + usage)
		}
	} else {
		id, err = parseSubcommandTaskID(fs, usage)
		if err != nil {
			return nil, 0, store.DecisionRequestInput{}, err
		}
	}
	input := store.DecisionRequestInput{
		Question:             strings.TrimSpace(*question),
		Reason:               strings.TrimSpace(*reason),
		RecommendationReason: strings.TrimSpace(*recommendationReason),
		Unverified:           unverifiedValues,
		OneFix:               strings.TrimSpace(*oneFix),
		DefaultAction:        strings.TrimSpace(*defaultAction),
		DefaultOption:        strings.TrimSpace(*defaultOption),
		DefaultTrigger:       strings.TrimSpace(*defaultTrigger),
		Doc:                  strings.TrimSpace(*doc),
		Refs:                 refValues,
		Grounds:              groundValues,
		RequestLane:          strings.TrimSpace(*requestLane),
		Supersedes:           strings.TrimSpace(*supersedes),
		Block:                *block,
		HumanOnly:            *humanOnly,
		AnswerPolicy:         strings.TrimSpace(*answerPolicy),
	}
	if len(optionValues) == 0 {
		return nil, 0, store.DecisionRequestInput{}, errors.New("at least one --option is required")
	}
	input.Options, err = parseTaskDecisionOptions(optionValues, *recommended, *noFreeAnswer)
	if err != nil {
		return nil, 0, store.DecisionRequestInput{}, err
	}
	if err := attachOptionMedia(&input.Options, attachmentValues); err != nil {
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

// attachOptionMedia appends each --option-attachment to the option it names,
// in the order the flags were given. A value is A|sha256 or
// A|sha256|caption; the store validates the address, the count and the
// caption, and refuses one that names no stored attachment.
func attachOptionMedia(options *store.DecisionOptions, values []string) error {
	for _, value := range values {
		parts := strings.SplitN(value, "|", 3)
		if len(parts) < 2 {
			return errors.New("--option-attachment must be A|sha256 or A|sha256|caption")
		}
		key, sha := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		caption := ""
		if len(parts) == 3 {
			caption = strings.TrimSpace(parts[2])
		}
		index := -1
		for i := range options.Options {
			if options.Options[i].Key == key {
				index = i
			}
		}
		if index < 0 {
			return fmt.Errorf("--option-attachment %s names no supplied --option key", key)
		}
		options.Options[index].Attachments = append(options.Options[index].Attachments, store.DecisionOptionAttachment{SHA256: sha, Caption: caption})
	}
	return nil
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
	rows, err := decisionTranslations(input, ko)
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
	if err := putDecisionTranslations(ctx, c, result.Request, rows); err != nil {
		output["translation_error"] = "translation_write_failed"
	}
	return printJSON(out, output)
}

// newTaskWriteFailed is writeFailed for --new-task, which has no task id yet.
// A refusal before any write is NOT recorded; anything else is UNKNOWN. A
// re-send that finds the task already filed is refused as a similar open task.
func newTaskWriteFailed(err error) error {
	if serverRefused(err) {
		return fmt.Errorf("decision ask --new-task NOT recorded: %v — do not notify that it is visible in the console", err)
	}
	return exitCodeError{code: exitWriteUnknown, err: fmt.Errorf("decision ask --new-task outcome UNKNOWN: %v — check `handoffkeep tasks list --project <project>` for the filed task before notifying; a re-send is refused as a similar open task rather than filing a second one", err)}
}

// decisionAskCmd records a decision request for the operator through the
// record-only route. Re-sending the same command after any failure creates
// nothing twice.
func decisionAskCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decisions ask", flag.ContinueOnError)
	ko := addDecisionTranslationFlags(fs)
	newTask := fs.Bool("new-task", false, "file a decide task in --project on --lane, titled with the question, then record the request on it (no task id)")
	project := fs.String("project", "", "project the --new-task is filed in")
	c, id, input, err := decisionRequestFlags(fs, decisionAskUsage, args)
	if err != nil {
		return notRecorded(err)
	}
	rows, err := decisionTranslations(input, ko)
	if err != nil {
		return notRecorded(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var recorded store.DecisionRequestResult
	if *newTask {
		if strings.TrimSpace(*project) == "" || input.RequestLane == "" {
			return notRecorded(errors.New("decisions ask --new-task needs --project and --lane"))
		}
		recorded, err = c.RecordDecisionRequestNewTask(ctx, store.NewTaskDecisionInput{Project: strings.TrimSpace(*project), Lane: input.RequestLane, Request: input})
		if err != nil {
			return newTaskWriteFailed(err)
		}
	} else {
		recorded, err = c.RecordDecisionRequest(ctx, id, input)
		if err != nil {
			return writeFailed("decision ask", id, err)
		}
	}
	var options store.DecisionOptions
	if recorded.Task.Refs.DecisionOptions != nil {
		options = *recorded.Task.Refs.DecisionOptions
	}
	request := recorded.Request
	translationErr := putDecisionTranslations(ctx, c, request, rows)
	output := map[string]any{
		"recorded":     true,
		"duplicate":    recorded.Duplicate,
		"task_id":      recorded.Task.ID,
		"request_id":   request.ID,
		"decision_ref": store.DecisionRef(request.ID),
		"ver":          store.DecisionRequestVer(request.Question, options, request.Doc, request.Refs),
		"status":       request.Status,
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
		// The recording lane lets hk skip the asker's copy of its own answer (hk 5527).
		in.RecordingLane = strings.TrimSpace(os.Getenv("PANEWIRE_LANE"))
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

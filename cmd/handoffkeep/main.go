package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/hubforward"
	hkmcp "github.com/mgh3326/handoffkeep/internal/mcp"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
	"github.com/mgh3326/handoffkeep/internal/ui"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var wrkErr wrkCLIError
		if errors.As(err, &wrkErr) {
			fmt.Fprintln(os.Stderr, "handoffkeep wrk: "+wrkErr.code)
			os.Exit(wrkrecords.Exit(wrkErr.code))
		}
		fmt.Fprintln(os.Stderr, "handoffkeep:", err)
		var exit exitCodeError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		os.Exit(1)
	}
}
func run(args []string, out, errout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: handoffkeep serve|sync|ledger|since|status|mcp|ctx|memory|doc|attach|r2usage|tasks|tickets|decisions|standing|dots|services|deploys|fleet-metrics|bench|changes|youtrack|version")
	}
	switch args[0] {
	case "wrk":
		return wrkCmd(args[1:], os.Stdin, out)
	case "sync":
		return syncCmd(args[1:], out)
	case "ledger":
		return ledgerCmd(args[1:], out)
	case "since":
		return sinceCmd(args[1:], out)
	case "status":
		return statusCmd(args[1:], out)
	case "serve":
		return serve(args[1:], errout)
	case "mcp":
		return runMCP()
	case "ctx":
		return ctxCmd(args[1:], out)
	case "memory":
		return memoryCmd(args[1:], out)
	case "doc":
		return docCmd(args[1:], out)
	case "attach":
		return attachCmd(args[1:], out)
	case "r2usage":
		return r2UsageCmd(args[1:], out)
	case "translations":
		return translationsCmd(args[1:], out)
	case "tasks":
		return tasksCmdE(args[1:], out, errout)
	case "tickets":
		return ticketsCmd(args[1:], out)
	case "standing":
		return standingCmd(args[1:], out)
	case "decisions":
		return decisionsCmd(args[1:], out)
	case "dots":
		return dotsCmd(args[1:], out)
	case "fleet-metrics":
		return fleetMetricsCmd(args[1:], out)
	case "bench":
		return benchCmd(args[1:], out)
	case "jobs":
		return jobsCmd(args[1:], out)
	case "services":
		return servicesCmd(args[1:], out)
	case "deploys":
		return deploysCmd(args[1:], out)
	case "changes":
		return changesCmd(args[1:], out)
	case "youtrack":
		return youtrackCmd(args[1:], out)
	case "version", "--version":
		return versionCmd(args[1:], out)
	default:
		return errors.New("usage: handoffkeep serve|sync|ledger|since|status|mcp|ctx|memory|doc|attach|r2usage|tasks|tickets|decisions|standing|dots|services|deploys|fleet-metrics|bench|changes|youtrack|version")
	}
}

// clientKeys are the env/config.env credentials every hk client is built
// from. Environment values win per key; config.env only fills keys the
// environment left unset.
type clientKeys struct {
	url, token                 string
	cfAccessID, cfAccessSecret string
}

func config() clientKeys {
	k := clientKeys{
		url:            os.Getenv("HANDOFFKEEP_URL"),
		token:          os.Getenv("HANDOFFKEEP_TOKEN"),
		cfAccessID:     os.Getenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID"),
		cfAccessSecret: os.Getenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET"),
	}
	if k.url != "" && k.token != "" && k.cfAccessID != "" && k.cfAccessSecret != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	b, _ := os.ReadFile(filepath.Join(home, ".config/handoffkeep/config.env"))
	for _, l := range strings.Split(string(b), "\n") {
		key, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch key {
		case "HANDOFFKEEP_URL":
			if k.url == "" {
				k.url = v
			}
		case "HANDOFFKEEP_TOKEN":
			if k.token == "" {
				k.token = v
			}
		case "HANDOFFKEEP_CF_ACCESS_CLIENT_ID":
			if k.cfAccessID == "" {
				k.cfAccessID = v
			}
		case "HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET":
			if k.cfAccessSecret == "" {
				k.cfAccessSecret = v
			}
		}
	}
	return k
}
func remoteClient(fs *flag.FlagSet) *remote.Client {
	k := config()
	c := &remote.Client{CFAccessClientID: k.cfAccessID, CFAccessClientSecret: k.cfAccessSecret}
	fs.StringVar(&c.URL, "url", k.url, "handoffkeep HTTP URL")
	fs.StringVar(&c.Token, "token", k.token, "handoffkeep bearer token")
	return c
}

// configuredClient builds the client for the paths that take no --url/--token
// flags — the mcp stdio server and the r2usage local comparison — so the
// optional CF Access service-token pair reaches those requests too.
func configuredClient() remote.Client {
	k := config()
	return remote.Client{URL: k.url, Token: k.token, CFAccessClientID: k.cfAccessID, CFAccessClientSecret: k.cfAccessSecret}
}
func mustClient(c *remote.Client) error {
	if c.URL == "" || c.Token == "" {
		return errors.New("HANDOFFKEEP_URL and HANDOFFKEEP_TOKEN are required (or ~/.config/handoffkeep/config.env)")
	}
	return remote.CFAccessPairError(c.CFAccessClientID, c.CFAccessClientSecret)
}
func printJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

type refFlags []string

func (r *refFlags) String() string         { return strings.Join(*r, ",") }
func (r *refFlags) Set(value string) error { *r = append(*r, value); return nil }

func checkpointRefs(values []string) (store.Refs, error) {
	refs := store.Refs{}
	for _, value := range values {
		key, ref, ok := strings.Cut(value, "=")
		if !ok || key == "" || ref == "" {
			return nil, errors.New("--ref must be k=v")
		}
		refs[key] = append(refs[key], ref)
	}
	return refs, nil
}

func taskRefs(pr, headSHA, reportPath, jobID string, linear *store.TaskLinear) (*store.TaskRefs, bool) {
	x := &store.TaskRefs{PR: pr, HeadSHA: headSHA, ReportPath: reportPath, JobID: jobID, Linear: linear}
	return x, pr != "" || headSHA != "" || reportPath != "" || jobID != "" || linear != nil
}

func normalizeTaskArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{"--url": true, "--token": true, "--lane": true, "--parent-lane": true, "--state": true, "--title": true, "--title-ko": true, "--body-ko": true, "--kind": true, "--priority": true, "--expected": true, "--by": true, "--to": true, "--closure": true, "--evidence": true, "--note": true, "--reason": true, "--question": true, "--limit": true, "--pr": true, "--head-sha": true, "--report-path": true, "--job-id": true, "--no-job": true, "--option": true, "--recommended": true, "--tier": true, "--grade": true, "--brief-key": true, "--label": true, "--linear-report-key": true, "--linear-verify-key": true, "--linear-decision-key": true, "--deploy-sha": true, "--origin-pr": true, "--origin-task": true, "--doc": true, "--project": true, "--area": true, "--parent": true, "--duplicate-of": true, "--link": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...), nil
}

// taskCreateRequirements is the hint tasks add appends to a server-side
// invalid_context rejection. The kind set comes from the server's own
// vocabulary in internal/store so the hint cannot drift from it.
func taskCreateRequirements() string {
	return "tasks add requires --lane <name> and --title <text>, and --kind must be one of " + strings.Join(store.TaskKinds(), ", ")
}

func tasksCmd(args []string, out io.Writer) error {
	return tasksCmdE(args, out, os.Stderr)
}

func tasksCmdE(args []string, out, errout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tasks add|list|export|claim|next|transition|reopen|rebind|relane|project|projects|owner|owners|milestone|label|labels|due|subscribe|unsubscribe|set-area|areas|parent|relate|unrelate|origin|migrate-states|decision-request|decision-resolve|show|comment|comments|disposition|event|sweep|project-status|intake|triage")
	}
	if args[0] == "disposition" {
		return dispositionCmd(args[1:], out)
	}
	if args[0] == "comment" || args[0] == "comments" {
		return taskCommentsCmd(args, os.Stdin, out)
	}
	if args[0] == "relane" {
		return taskRelaneCmd(args, os.Stdin, out)
	}
	if args[0] == "rebind" {
		return taskRebindCmd(args[1:], out)
	}
	if args[0] == "parent" {
		return taskParentCmd(args[1:], out)
	}
	if args[0] == "relate" || args[0] == "unrelate" {
		return taskRelationCmd(args, out)
	}
	if args[0] == "origin" {
		return taskOriginCmd(args[1:], out)
	}
	if args[0] == "migrate-states" {
		return taskMigrateStatesCmd(args[1:], out)
	}
	if args[0] == "project-status" {
		return projectStatusCmd(args[1:], out)
	}
	if args[0] == "project" || args[0] == "projects" {
		return taskProjectCmd(args, out)
	}
	if args[0] == "owner" || args[0] == "owners" {
		return taskOwnerCmd(args, out)
	}
	if args[0] == "milestone" || args[0] == "label" || args[0] == "labels" || args[0] == "due" || args[0] == "subscribe" || args[0] == "unsubscribe" {
		return taskModelCmd(args, out)
	}
	if args[0] == "set-area" || args[0] == "areas" {
		return taskAreaCmd(args, out)
	}
	if args[0] == "decision-request" {
		return decisionRequestCmd(args, out)
	}
	if args[0] == "decision-resolve" {
		return decisionResolveCmd(args, out, errout)
	}
	if args[0] == "event" {
		return taskEventCmd(args[1:], out)
	}
	if args[0] == "sweep" {
		return taskSweepCmd(args[1:], out)
	}
	if args[0] == "intake" {
		return taskIntakeCmd(args[1:], out)
	}
	if args[0] == "triage" {
		return taskTriageCmd(args[1:], out)
	}
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	lane := fs.String("lane", "", "task lane")
	parentLane := fs.String("parent-lane", "", "parent lane")
	state := fs.String("state", "", "task state (triage, backlog, unstarted, researching, developing, reviewing, merged, deployed, completed, dropped, duplicate, icebox; old names such as in_progress or hold are still accepted)")
	parentID := fs.Int64("parent", 0, "parent task id (tasks add only)")
	duplicateOf := fs.Int64("duplicate-of", 0, "task a duplicate transition points at (tasks transition --to duplicate)")
	link := fs.Int64("link", 0, "existing task to relate the new task to as related, answering the duplicate check (tasks add only)")
	newTask := fs.Bool("new", false, "file the task without the duplicate check (tasks add only)")
	set := fs.Bool("set", false, "apply a triage exit to the task's children still in triage (tasks transition only)")
	project := fs.String("project", "", "task project (required on add; an explicit empty value lists legacy no-project rows on list/export)")
	area := fs.String("area", "", "task area from the closed vocabulary listed by tasks areas (optional on add; an explicit empty value lists no-area rows on list)")
	title := fs.String("title", "", "task title")
	titleKo := fs.String("title-ko", "", "Korean title translation")
	bodyKo := fs.String("body-ko", "", "Korean body translation")
	kind := fs.String("kind", "implement", "task kind")
	priority := fs.Int("priority", 0, "higher is first")
	expected := fs.String("expected", "", "expected task duration (for example 4h or 1d; tasks add only)")
	by := fs.String("by", "", "session label")
	to := fs.String("to", "", "target state")
	closure := fs.String("closure", "", "closure type")
	evidence := fs.String("evidence", "", "closure evidence")
	note := fs.String("note", "", "transition note")
	reason := fs.String("reason", "", "reason required for reopen")
	question := fs.String("question", "", "question required for needs_decision")
	limit := fs.Int("limit", 20, "maximum results")
	stalled := fs.Bool("stalled", false, "list only stalled tasks: started, not awaiting a decision, no activity within the server's started_stalled_hours (tasks list only)")
	pr := fs.String("pr", "", "pull request reference")
	headSHA := fs.String("head-sha", "", "head revision reference")
	reportPath := fs.String("report-path", "", "report path reference")
	jobID := fs.String("job-id", "", "job reference")
	noJob := fs.String("no-job", "", "recorded reason a transition into a started state may run without refs.job_id (exclusive with --job-id)")
	originPR := fs.String("origin-pr", "", "merged pull request this task came from")
	originTask := fs.Int64("origin-task", 0, "task this task came from (a disposition item for an ordered follow-up)")
	bodyDoc := fs.String("doc", "", "hk document key holding the task body: key or key#section (tasks add only)")
	tier := fs.String("tier", "", "Linear task tier metadata (T0..T3)")
	grade := fs.String("grade", "", "Linear task grade metadata (S+..C)")
	briefKey := fs.String("brief-key", "", "handoffkeep brief document key for the Linear issue")
	linearSync := fs.Bool("linear-sync", false, "retain legacy Linear task metadata (sync connector retired)")
	linearReportKey := fs.String("linear-report-key", "", "handoffkeep report document key for a terminal Linear comment")
	linearVerifyKey := fs.String("linear-verify-key", "", "independent verification report key for a terminal Linear comment")
	linearDecisionKey := fs.String("linear-decision-key", "", "decision document key for a terminal Linear comment")
	deploySHA := fs.String("deploy-sha", "", "deployed revision for a terminal Linear comment")
	var linearLabels refFlags
	fs.Var(&linearLabels, "label", "existing Linear label name (repeatable; labels are never created)")
	var optionValues refFlags
	fs.Var(&optionValues, "option", "decision option A|label (repeatable)")
	recommended := fs.String("recommended", "", "recommended decision option key")
	noFreeAnswer := fs.Bool("no-free-answer", false, "disallow a free-form answer")
	parseArgs, err := normalizeTaskArgs(args[1:])
	if err != nil {
		return err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	linearFlagsUsed, docFlagUsed, noJobSet, projectSet, areaSet, expectedSet := false, false, false, false, false, false
	parentSet, duplicateOfSet, stalledSet := false, false, false
	linkSet, kindSet, prioritySet := false, false, false
	closureSet := false
	fs.Visit(func(item *flag.Flag) {
		switch item.Name {
		case "closure", "evidence":
			closureSet = true
		case "link":
			linkSet = true
		case "kind":
			kindSet = true
		case "priority":
			prioritySet = true
		}
		if item.Name == "stalled" {
			stalledSet = true
		}
		if item.Name == "parent" {
			parentSet = true
		}
		if item.Name == "duplicate-of" {
			duplicateOfSet = true
		}
		if item.Name == "expected" {
			expectedSet = true
		}
		if item.Name == "doc" {
			docFlagUsed = true
		}
		if item.Name == "no-job" {
			noJobSet = true
		}
		if item.Name == "project" {
			projectSet = true
		}
		if item.Name == "area" {
			areaSet = true
		}
		switch item.Name {
		case "linear-sync", "tier", "grade", "brief-key", "label", "linear-report-key", "linear-verify-key", "linear-decision-key", "deploy-sha":
			linearFlagsUsed = true
		}
	})
	noJobReason := ""
	if noJobSet {
		// An explicit empty flag is a caller mistake, not a request for the
		// exemption: refuse before any request is sent.
		noJobReason = strings.TrimSpace(*noJob)
		if noJobReason == "" {
			return errors.New("--no-job requires a non-empty reason")
		}
		switch args[0] {
		case "claim", "next", "transition":
		default:
			return fmt.Errorf("--no-job is not accepted by tasks %s", args[0])
		}
	}
	var linearRefs *store.TaskLinear
	if linearFlagsUsed {
		linearRefs = &store.TaskLinear{
			Sync:      *linearSync,
			Tier:      *tier,
			Grade:     *grade,
			Brief:     *briefKey,
			Labels:    append([]string{}, linearLabels...),
			Report:    *linearReportKey,
			Verify:    *linearVerifyKey,
			Decision:  *linearDecisionKey,
			DeploySHA: *deploySHA,
		}
	}
	// A task body is attached only when the task is filed; no other
	// subcommand writes body_doc, so the flag must not be silently ignored.
	if expectedSet && (args[0] != "add" || !store.ValidExpected(*expected)) {
		return errors.New("--expected requires a positive duration and is accepted only by tasks add")
	}
	if parentSet && (args[0] != "add" || *parentID < 1) {
		return errors.New("--parent requires a positive task id and is accepted only by tasks add (tasks parent changes an existing task)")
	}
	if linkSet && (args[0] != "add" || *link < 1) {
		return errors.New("--link requires a positive task id and is accepted only by tasks add")
	}
	if *newTask && args[0] != "add" {
		return errors.New("--new is accepted only by tasks add")
	}
	if linkSet && *newTask {
		return errors.New("--link and --new are exclusive")
	}
	if closureSet && args[0] != "transition" {
		return errors.New("--closure/--evidence are accepted only by tasks transition")
	}
	if *set && args[0] != "transition" {
		return errors.New("--set is accepted only by tasks transition")
	}
	if duplicateOfSet && (args[0] != "transition" || *duplicateOf < 1) {
		return errors.New("--duplicate-of requires a positive task id and is accepted only by tasks transition")
	}
	if stalledSet && args[0] != "list" {
		return errors.New("--stalled is accepted only by tasks list")
	}
	if docFlagUsed && args[0] != "add" {
		return errors.New("--doc is accepted only by tasks add")
	}
	if areaSet && args[0] != "add" && args[0] != "list" {
		return errors.New("--area is accepted only by tasks add and tasks list (tasks set-area changes an existing task)")
	}
	// The vocabulary is a code constant shared with the server, so a bad
	// value is refused here and no request is sent.
	if areaSet && (args[0] != "list" || *area != "") {
		if err := store.CheckTaskArea(area); err != nil {
			return err
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	parseID := func() (int64, error) {
		if fs.NArg() != 1 {
			return 0, errors.New("task command requires id")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return 0, errors.New("task id must be positive")
		}
		return id, nil
	}
	switch args[0] {
	case "add":
		if fs.NArg() != 0 {
			return errors.New("tasks add takes flags only")
		}
		if !projectSet || strings.TrimSpace(*project) == "" {
			return errors.New("tasks add requires --project <name> (the allowed set is listed by tasks projects)")
		}
		// Shape only: the document may be written after the task is filed.
		if *bodyDoc != "" && !store.ValidBodyDoc(*bodyDoc) {
			return errors.New("--doc must be a document key or key#section")
		}
		refs, _ := taskRefs(*pr, *headSHA, *reportPath, *jobID, linearRefs)
		refs.OriginPR, refs.OriginTask = *originPR, *originTask
		var areaTag *string
		if areaSet {
			areaTag = area
		}
		var expectedDuration *string
		if expectedSet {
			expectedDuration = expected
		}
		var parent *int64
		if *parentID > 0 {
			parent = parentID
		}
		var originalBody string
		if *bodyKo != "" {
			if *bodyDoc == "" {
				return errors.New("--body-ko requires --doc")
			}
			doc, found, err := c.GetDocument(ctx, strings.SplitN(*bodyDoc, "#", 2)[0])
			if err != nil {
				return err
			}
			if !found {
				return errors.New("--body-ko requires an existing document")
			}
			originalBody = doc.Body
		}
		x, err := c.CreateTaskChecked(ctx, store.Task{ParentID: parent, Expected: expectedDuration, Lane: *lane, ParentLane: *parentLane, Title: *title, Kind: *kind, Priority: *priority, Refs: *refs, BodyDoc: *bodyDoc, Project: project, Area: areaTag}, store.DuplicateCheck{Link: *link, New: *newTask})
		if err != nil {
			// invalid_context on create is a validation failure; append the
			// add requirements rather than leave a bare code to diagnose.
			var he *remote.HTTPError
			if errors.As(err, &he) && he.Code == "invalid_context" {
				return fmt.Errorf("%w — %s", err, taskCreateRequirements())
			}
			if remote.ErrorCode(err) == "similar_tasks" {
				return fmt.Errorf("%w — pass --link <id> to relate the new task to one of them, or --new to file it anyway", err)
			}
			return err
		}
		rows := []store.Translation{}
		if *titleKo != "" {
			rows = append(rows, store.Translation{Entity: "task", EntityID: strconv.FormatInt(x.ID, 10), Field: "title", Lang: "ko", SourceSHA256: store.SourceSHA256(x.Title), Text: *titleKo, Translator: "cli"})
		}
		if *bodyKo != "" {
			rows = append(rows, store.Translation{Entity: "task", EntityID: strconv.FormatInt(x.ID, 10), Field: "body", Lang: "ko", SourceSHA256: store.SourceSHA256(originalBody), Text: *bodyKo, Translator: "cli"})
		}
		translationError := ""
		if len(rows) > 0 {
			if _, err := c.PutTranslations(ctx, rows); err != nil {
				translationError = "translation_write_failed"
			}
		}
		return printJSON(out, struct {
			store.Task
			TranslationError string `json:"translation_error,omitempty"`
		}{x, translationError})
	case "list":
		if fs.NArg() != 0 {
			return errors.New("tasks list takes flags only")
		}
		var projectFilter *string
		if projectSet {
			projectFilter = project
		}
		var areaFilter *string
		if areaSet {
			areaFilter = area
		}
		var xs []store.Task
		if *stalled {
			xs, err = c.ListTasksFiltered(ctx, store.TaskQuery{Lane: *lane, State: *state, ParentLane: *parentLane, Project: projectFilter, Area: areaFilter, Stalled: true, Limit: *limit})
		} else {
			xs, err = c.ListTasks(ctx, *lane, *state, *parentLane, projectFilter, areaFilter, *limit)
		}
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"tasks": xs})
	case "export":
		if fs.NArg() != 0 {
			return errors.New("tasks export takes flags only")
		}
		// An unset --limit leaves the route's own default in force rather than
		// silently exporting fewer rows. An explicitly set --limit outside the
		// route's range is rejected here so an invalid export never leaves the
		// process.
		exportLimit := 0
		limitSet := false
		fs.Visit(func(item *flag.Flag) {
			if item.Name == "limit" {
				limitSet = true
				exportLimit = *limit
			}
		})
		if limitSet && (exportLimit < 1 || exportLimit > store.ExportLimitMax) {
			return fmt.Errorf("invalid_export_query: limit must be between 1 and %d", store.ExportLimitMax)
		}
		var projectFilter *string
		if projectSet {
			projectFilter = project
		}
		doc, err := c.ExportTasks(ctx, *lane, *state, *parentLane, projectFilter, exportLimit)
		if err != nil {
			return err
		}
		_, err = out.Write(doc)
		return err
	case "claim":
		id, err := parseID()
		if err != nil {
			return err
		}
		if noJobSet && *jobID != "" {
			return errors.New("--job-id and --no-job are exclusive")
		}
		x, err := c.ClaimTask(ctx, id, *by, *jobID, noJobReason)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "next":
		if fs.NArg() != 0 {
			return errors.New("tasks next takes flags only")
		}
		if noJobSet && *jobID != "" {
			return errors.New("--job-id and --no-job are exclusive")
		}
		x, err := c.NextTask(ctx, *lane, *by, *jobID, noJobReason)
		if err != nil {
			if remote.ErrorCode(err) == "queue_empty" {
				return exitCodeError{code: 3, err: errors.New("no backlog task")}
			}
			return err
		}
		return printJSON(out, x)
	case "reopen":
		id, err := parseID()
		if err != nil {
			return err
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("tasks reopen requires --reason")
		}
		x, err := c.ReopenTask(ctx, id, *reason)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "transition":
		id, err := parseID()
		if err != nil {
			return err
		}
		if noJobSet && *jobID != "" {
			return errors.New("--job-id and --no-job are exclusive")
		}
		usingOptions := len(optionValues) > 0 || *recommended != "" || *noFreeAnswer
		if usingOptions && *to != "needs_decision" {
			return errors.New("decision option flags require --to needs_decision")
		}
		if *to == "needs_decision" {
			if strings.TrimSpace(*question) == "" {
				return errors.New("tasks transition --to needs_decision requires --question")
			}
			if *note == "" {
				*note = *question
			}
		}
		refs, hasRefs := taskRefs(*pr, *headSHA, *reportPath, *jobID, linearRefs)
		if usingOptions {
			options, err := parseTaskDecisionOptions(optionValues, *recommended, *noFreeAnswer)
			if err != nil {
				return err
			}
			if len([]byte(*note+"\n"+store.FormatDecisionOptions(options))) > store.RelayLaneEventMaxBytes {
				return errors.New("decision question and options exceed 2048 bytes")
			}
			refs.DecisionOptions = &options
			hasRefs = true
		}
		if !hasRefs {
			refs = nil
		}
		in := store.TransitionInput{To: *to, Closure: *closure, Evidence: *evidence, Note: *note, Refs: refs, NoJob: noJobReason, DuplicateOf: *duplicateOf, Set: *set}
		// Leaving triage may carry the values the gate needs; only flags the
		// caller set are sent.
		if prioritySet {
			in.Priority = priority
		}
		if kindSet {
			in.Kind = *kind
		}
		if projectSet {
			in.Project = project
		}
		x, err := c.TransitionTaskWith(ctx, id, in)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "show":
		id, err := parseID()
		if err != nil {
			return err
		}
		x, found, err := c.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("not_found")
		}
		return printJSON(out, x)
	default:
		return errors.New("usage: tasks add|list|export|claim|next|transition|reopen|rebind|relane|project|projects|owner|owners|set-area|areas|parent|relate|unrelate|migrate-states|decision-request|decision-resolve|show|comment|comments|disposition|intake|triage")
	}
}

func parseTaskDecisionOptions(values []string, recommended string, noFreeAnswer bool) (store.DecisionOptions, error) {
	if len(values) == 0 {
		if recommended != "" {
			return store.DecisionOptions{}, errors.New("--recommended requires at least one --option")
		}
		if noFreeAnswer {
			return store.DecisionOptions{}, errors.New("--no-free-answer requires at least one --option")
		}
		return store.DecisionOptions{}, errors.New("decision options are required")
	}
	if len(values) > 6 {
		return store.DecisionOptions{}, errors.New("at most six --option values are allowed")
	}
	options := store.DecisionOptions{AllowFree: !noFreeAnswer}
	seen := map[string]bool{}
	for _, value := range values {
		key, label, found := strings.Cut(value, "|")
		key, label = strings.TrimSpace(key), strings.TrimSpace(label)
		if !found || strings.Contains(label, "|") || seen[key] {
			return store.DecisionOptions{}, errors.New("--option must be a unique A|label value")
		}
		seen[key] = true
		options.Options = append(options.Options, store.DecisionOption{Key: key, Label: label})
	}
	if recommended != "" {
		if len(recommended) != 1 || recommended[0] < 'A' || recommended[0] > 'F' || !seen[recommended] {
			return store.DecisionOptions{}, errors.New("--recommended must name a supplied option key")
		}
		for index := range options.Options {
			options.Options[index].Recommended = options.Options[index].Key == recommended
		}
	}
	// The detailed validator names the failing rule, e.g. a label over the
	// 120-byte limit (bytes, not characters).
	if err := store.CheckDecisionOptionsDetailed(options); err != nil {
		return store.DecisionOptions{}, err
	}
	return options, nil
}

func normalizeDecisionArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{"--url": true, "--token": true, "--by": true, "--answer": true, "--note": true, "--no-job": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...), nil
}

func decisionsCmd(args []string, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "ask":
			return decisionAskCmd(args[1:], out)
		case "list":
			return decisionListCmd(args[1:], out)
		case "snooze":
			return decisionSnoozeCmd(args[1:], out)
		case "backfill":
			return decisionBackfillCmd(args[1:], out)
		case "hygiene":
			return decisionHygieneCmd(args[1:], out)
		}
	}
	if len(args) == 0 || args[0] != "resolve" {
		return errors.New("usage: decisions resolve <task|escalation|lane> <id> --by <lane> --answer <answer> [--note <note>] [--no-inject]\n       " + decisionResolveRequestUsage + "\n       " + decisionAskUsage + "\n       " + decisionListUsage + "\n       " + decisionSnoozeUsage + "\n       " + decisionBackfillUsage + "\n       " + decisionHygieneUsage)
	}
	if len(args) > 1 && args[1] == "request" {
		return decisionResolveRequestCmd(args[2:], out)
	}
	fs := flag.NewFlagSet("decisions resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	by := fs.String("by", "", "answering lane")
	answer := fs.String("answer", "", "decision answer")
	note := fs.String("note", "", "optional decision note")
	noInject := fs.Bool("no-inject", false, "mark the event delivered without injection")
	noJob := fs.String("no-job", "", "recorded reason for the resolved task (kept for old servers; the answer enters no started state)")
	parseArgs, err := normalizeDecisionArgs(args[1:])
	if err != nil {
		return err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("decisions resolve requires a type and id")
	}
	id, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil || id < 1 {
		return errors.New("decision id must be positive")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	event, err := c.ResolveDecision(ctx, fs.Arg(0), id, *by, *answer, *note, *noInject, strings.TrimSpace(*noJob))
	if err != nil {
		return err
	}
	return printJSON(out, event)
}

type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string { return e.err.Error() }

// searchArgs permits the query before or after flags. The standard flag package
// stops parsing at a positional argument, so normalize its known value flags.
func searchArgs(args []string) ([]string, []string, error) {
	valueFlags := map[string]bool{"--url": true, "--token": true, "--session": true, "--limit": true, "--kind": true, "--scope": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] {
				i++
				if i >= len(args) {
					return nil, nil, fmt.Errorf("%s requires a value", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return flags, positional, nil
}

func ctxCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: ctx checkpoint|recent|search")
	}
	fs := flag.NewFlagSet("ctx "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	session := fs.String("session", "", "session")
	limit := fs.Int("limit", 3, "limit")
	kind := fs.String("kind", "", "checkpoint kind")
	title := fs.String("title", "", "title")
	body := fs.String("body", "", "body")
	file := fs.String("file", "", "body file")
	scope := fs.String("scope", "all", "scope")
	var refs refFlags
	fs.Var(&refs, "ref", "checkpoint reference k=v (repeatable)")
	parseArgs := args[1:]
	var searchQuery []string
	if args[0] == "search" {
		var e error
		parseArgs, searchQuery, e = searchArgs(parseArgs)
		if e != nil {
			return e
		}
	}
	if e := fs.Parse(parseArgs); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "checkpoint":
		if *kind == "" {
			*kind = "checkpoint"
		}
		if *file != "" {
			b, e := os.ReadFile(*file)
			if e != nil {
				return e
			}
			*body = string(b)
		}
		checkpointRefs, e := checkpointRefs(refs)
		if e != nil {
			return e
		}
		v, e := c.Checkpoint(ctx, "", store.Checkpoint{Session: *session, Kind: *kind, Title: *title, Body: *body, Refs: checkpointRefs})
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "recent":
		v, e := c.Recent(ctx, *session, *kind, *limit)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "search":
		if len(searchQuery) != 1 {
			return errors.New("ctx search requires query")
		}
		// The 3-per-query default made discovery unusable (#551); search now
		// defaults to 20 rows for every scope and the store marks a page cut
		// at its cap with truncated=true on every row.
		limitSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "limit" {
				limitSet = true
			}
		})
		if !limitSet {
			*limit = 20
		}
		v, e := c.Search(ctx, searchQuery[0], *scope, *session, *limit)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	default:
		return errors.New("usage: ctx checkpoint|recent|search")
	}
}

func memoryCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: memory push|pull|list")
	}
	fs := flag.NewFlagSet("memory "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	agent := fs.String("agent", "", "agent")
	dir := fs.String("dir", "", "directory")
	apply := fs.Bool("apply", false, "apply changes")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		v, e := c.ListMemory(ctx, *agent, false)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "push":
		items, e := readMem(*agent, *dir)
		if e != nil {
			return e
		}
		if !*apply {
			return printJSON(out, map[string]any{"dry_run": true, "count": len(items)})
		}
		for _, x := range items {
			if _, e = c.PutMemory(ctx, "", x); e != nil {
				return e
			}
		}
		return printJSON(out, map[string]any{"pushed": len(items)})
	case "pull":
		items, e := c.ListMemory(ctx, *agent, true)
		if e != nil {
			return e
		}
		if !*apply {
			return printJSON(out, map[string]any{"dry_run": true, "count": len(items)})
		}
		if e = writeMem(*dir, items); e != nil {
			return e
		}
		return printJSON(out, map[string]any{"pulled": len(items)})
	default:
		return errors.New("usage: memory push|pull|list")
	}
}
func readMem(agent, dir string) ([]store.Memory, error) {
	ents, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	out := []store.Memory{}
	for _, x := range ents {
		if x.IsDir() || !strings.HasSuffix(x.Name(), ".md") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, x.Name()))
		if e != nil {
			return nil, e
		}
		m, e := parseMem(agent, strings.TrimSuffix(x.Name(), ".md"), string(b))
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func parseMem(agent, fallback, raw string) (store.Memory, error) {
	m := store.Memory{Agent: agent, Name: fallback, Type: "project", Content: raw}
	if !strings.HasPrefix(raw, "---\n") {
		return m, nil
	}
	parts := strings.SplitN(raw, "---\n", 3)
	if len(parts) != 3 {
		return m, errors.New("invalid memory frontmatter")
	}
	for _, l := range strings.Split(parts[1], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			m.Name = strings.TrimSpace(v)
		case "description":
			m.Description = strings.TrimSpace(v)
		case "type":
			m.Type = strings.TrimSpace(v)
		}
	}
	return m, nil
}
func writeMem(dir string, items []store.Memory) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	names := []string{}
	for _, x := range items {
		raw := x.Content
		if !strings.HasPrefix(raw, "---\n") {
			raw = fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n%s", x.Name, x.Description, x.Type, x.Content)
		}
		if e := os.WriteFile(filepath.Join(dir, x.Name+".md"), []byte(raw), 0600); e != nil {
			return e
		}
		names = append(names, fmt.Sprintf("- [%s](%s.md) — %s", x.Name, x.Name, x.Description))
	}
	sort.Strings(names)
	return os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Join(names, "\n")+"\n"), 0600)
}

func docCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: doc put|get|list|import")
	}
	fs := flag.NewFlagSet("doc "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c *remote.Client
	if args[0] == "import" && hasFlag(args[1:], "manifest-out") {
		// Candidate generation is intentionally local-only. Avoid even loading
		// handoffkeep credentials when no service call can be made.
		c = &remote.Client{}
		fs.StringVar(&c.URL, "url", "", "handoffkeep HTTP URL")
		fs.StringVar(&c.Token, "token", "", "handoffkeep bearer token")
	} else {
		c = remoteClient(fs)
	}
	key := fs.String("key", "", "key")
	docID := fs.String("id", "", "document id")
	kind := fs.String("kind", "", "kind")
	session := fs.String("session", "", "session")
	job := fs.String("job", "", "job")
	task := fs.Int64("task", 0, "task id the document belongs to (put) or to filter by (list)")
	originClass := fs.String("origin-class", "", "how the document came to be: authored, job or import (put)")
	file := fs.String("file", "", "file")
	prefix := fs.String("prefix", "", "prefix")
	dir := fs.String("dir", "", "directory")
	glob := fs.String("glob", "**/*.md", "glob")
	manifest := fs.String("manifest", "", "JSON import manifest")
	manifestOut := fs.String("manifest-out", "", "write a candidate JSON manifest without importing")
	apply := fs.Bool("apply", false, "apply changes")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	taskSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "task" {
			taskSet = true
		}
	})
	if taskSet && *task < 1 {
		return errors.New("--task must be a positive task id")
	}
	if (taskSet && args[0] != "put" && args[0] != "list") || (*originClass != "" && args[0] != "put") {
		return errors.New("--task applies to doc put and doc list, --origin-class to doc put")
	}
	ctx := context.Background()
	if args[0] == "import" && *manifestOut != "" {
		if *manifest != "" || *apply {
			return errors.New("--manifest-out cannot be combined with --manifest or --apply")
		}
		return writeImportManifest(*dir, *glob, *manifestOut, out)
	}
	if e := mustClient(c); e != nil {
		return e
	}
	switch args[0] {
	case "put":
		if *kind == "" {
			*kind = "note"
		}
		b, e := os.ReadFile(*file)
		if e != nil {
			return e
		}
		doc := store.Document{Key: *key, Kind: *kind, Session: *session, Job: *job, Body: string(b), OriginClass: *originClass}
		if taskSet {
			doc.TaskID = task
		}
		v, changed, e := c.PutDocument(ctx, "", doc)
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"document": v, "changed": changed})
	case "get":
		idSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "id" {
				idSet = true
			}
		})
		if idSet {
			if fs.NArg() != 0 {
				return errors.New("doc get --id cannot be combined with a key")
			}
			id, e := parseDocumentID(*docID)
			if e != nil {
				return e
			}
			v, ok, e := c.GetDocumentByID(ctx, id)
			if e != nil {
				return e
			}
			if !ok {
				return errors.New("not_found")
			}
			return printJSON(out, v)
		}
		if fs.NArg() != 1 {
			return errors.New("doc get requires key")
		}
		v, ok, e := c.GetDocument(ctx, fs.Arg(0))
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("not_found")
		}
		return printJSON(out, v)
	case "list":
		v, e := c.ListDocuments(ctx, *prefix, *kind, *session, *task, 100)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "import":
		if *manifest != "" {
			if *dir != "" {
				return errors.New("--manifest cannot be combined with --dir")
			}
			return importDocsManifest(ctx, c, *manifest, *apply, out)
		}
		return importDocs(ctx, c, *dir, *glob, *apply, out)
	default:
		return errors.New("usage: doc put|get|list|import")
	}
}

var docIDArg = regexp.MustCompile(`^#?([0-9]+)$`)

// parseDocumentID accepts a bare number or "#<n>" and rejects anything else —
// a mistyped key must fail loudly rather than be looked up as an id.
func parseDocumentID(arg string) (int64, error) {
	m := docIDArg.FindStringSubmatch(strings.TrimSpace(arg))
	if m == nil {
		return 0, errors.New("invalid document id")
	}
	id, e := strconv.ParseInt(m[1], 10, 64)
	if e != nil || id < 1 {
		return 0, errors.New("invalid document id")
	}
	return id, nil
}

func hasFlag(args []string, name string) bool {
	want := "--" + name
	for _, arg := range args {
		if arg == want || strings.HasPrefix(arg, want+"=") {
			return true
		}
	}
	return false
}

type docImportManifestEntry struct {
	Key    string `json:"key"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Kind   string `json:"kind"`
}

type docImportManifestItemResult struct {
	Key     string `json:"key"`
	Status  string `json:"status"`
	Pattern string `json:"pattern,omitempty"`
	Error   string `json:"error,omitempty"`
}

type docImportManifestResult struct {
	DryRun bool                          `json:"dry_run"`
	Items  []docImportManifestItemResult `json:"items"`
}

type docImportManifestSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

var (
	robManifestName = regexp.MustCompile(`^ROB-(\d+)`)
	robManifestKey  = regexp.MustCompile(`^linear/ROB-\d+$`)
	manifestSHA256  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func writeImportManifest(dir, glob, destination string, out io.Writer) error {
	if dir == "" || destination == "" {
		return errors.New("doc import --manifest-out requires --dir and an output file")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	entries := []docImportManifestEntry{}
	skipped := []docImportManifestSkipped{}
	byKey := map[string]string{}
	err = filepath.WalkDir(absDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		relative, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if glob != "**/*.md" {
			matched, matchErr := filepath.Match(glob, relative)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				return nil
			}
		}
		match := robManifestName.FindStringSubmatch(d.Name())
		if len(match) != 2 {
			skipped = append(skipped, docImportManifestSkipped{Path: relative, Reason: "basename does not match ^ROB-(\\d+)"})
			return nil
		}
		key := "linear/ROB-" + match[1]
		if previous, exists := byKey[key]; exists {
			return fmt.Errorf("duplicate generated manifest key %q: %q and %q", key, previous, path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		byKey[key] = path
		entries = append(entries, docImportManifestEntry{Key: key, Path: path, SHA256: sha256Hex(body), Kind: "note"})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(destination, raw, 0600); err != nil {
		return err
	}
	return printJSON(out, map[string]any{
		"dry_run": true, "manifest_out": destination, "matched": len(entries), "skipped": skipped,
	})
}

func readImportManifest(path string) ([]docImportManifestEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var entries []docImportManifestEntry
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("manifest contains trailing JSON values")
		}
		return nil, err
	}
	byKey := map[string]string{}
	for _, entry := range entries {
		if !robManifestKey.MatchString(entry.Key) || !filepath.IsAbs(entry.Path) || !manifestSHA256.MatchString(entry.SHA256) || entry.Kind != "note" {
			return nil, fmt.Errorf("invalid manifest entry for key %q", entry.Key)
		}
		if previous, exists := byKey[entry.Key]; exists {
			return nil, fmt.Errorf("duplicate manifest key %q: %q and %q", entry.Key, previous, entry.Path)
		}
		byKey[entry.Key] = entry.Path
	}
	return entries, nil
}

func importDocsManifest(ctx context.Context, c *remote.Client, path string, apply bool, out io.Writer) error {
	entries, err := readImportManifest(path)
	if err != nil {
		return err
	}
	result := docImportManifestResult{DryRun: !apply, Items: make([]docImportManifestItemResult, 0, len(entries))}
	for _, entry := range entries {
		item := docImportManifestItemResult{Key: entry.Key}
		body, err := os.ReadFile(entry.Path)
		if err != nil {
			return err
		}
		actualSHA := sha256Hex(body)
		if !strings.EqualFold(actualSHA, entry.SHA256) {
			item.Status = "sha_mismatch"
			item.Error = fmt.Sprintf("expected %s, got %s", strings.ToLower(entry.SHA256), actualSHA)
			result.Items = append(result.Items, item)
			continue
		}
		existing, found, err := c.GetDocument(ctx, entry.Key)
		if err != nil {
			return err
		}
		if found {
			existingSHA := existing.SHA256
			if existingSHA == "" {
				existingSHA = sha256Hex([]byte(existing.Body))
			}
			if !strings.EqualFold(existingSHA, actualSHA) || existing.Kind != entry.Kind {
				item.Status = "conflict"
				item.Error = "existing document body or kind differs"
			} else {
				item.Status = "unchanged"
			}
			result.Items = append(result.Items, item)
			continue
		}
		if !apply {
			item.Status = "created"
			result.Items = append(result.Items, item)
			continue
		}
		_, changed, err := c.PutDocument(ctx, "", store.Document{Key: entry.Key, Kind: entry.Kind, Body: string(body)})
		if err != nil {
			if pattern, rejected := remote.SecretPattern(err); rejected {
				item.Status = "rejected"
				item.Pattern = pattern
				result.Items = append(result.Items, item)
				continue
			}
			return err
		}
		if changed {
			item.Status = "created"
		} else {
			item.Status = "unchanged"
		}
		result.Items = append(result.Items, item)
	}
	return printJSON(out, result)
}

func importDocs(ctx context.Context, c *remote.Client, dir, glob string, apply bool, out io.Writer) error {
	if dir == "" {
		return errors.New("doc import requires --dir")
	}
	n, rejected := 0, []map[string]string{}
	e := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		key, _ := filepath.Rel(dir, path)
		key = filepath.ToSlash(key)
		if glob != "**/*.md" {
			ok, _ := filepath.Match(glob, key)
			if !ok {
				return nil
			}
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		kind, job := importKind(key)
		if !apply {
			n++
			return nil
		}
		_, _, e = c.PutDocument(ctx, "", store.Document{Key: key, Kind: kind, Job: job, Body: string(b)})
		if e != nil {
			if p, ok := remote.SecretPattern(e); ok {
				rejected = append(rejected, map[string]string{"key": key, "pattern": p})
				return nil
			}
			return e
		}
		n++
		return nil
	})
	if e != nil {
		return e
	}
	return printJSON(out, map[string]any{"dry_run": !apply, "matched": n, "rejected": rejected})
}
func importKind(key string) (string, string) {
	p := strings.Split(key, "/")
	job := ""
	if len(p) >= 3 && p[0] == "jobs" {
		job = p[1]
	}
	base := filepath.Base(key)
	if base == "brief.md" {
		return "brief", job
	}
	if base == "report.md" {
		return "report", job
	}
	if strings.HasPrefix(base, "answer-") {
		return "answer", job
	}
	return "note", job
}

func attachCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: attach put|get|list|usage")
	}
	fs := flag.NewFlagSet("attach "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	checkpoint := fs.String("checkpoint", "", "checkpoint id")
	doc := fs.String("doc", "", "document key")
	memory := fs.String("memory", "", "memory agent/name")
	name := fs.String("name", "", "original filename")
	output := fs.String("o", "", "output path")
	// Permit the documented `attach get SHA -o file` form as well as flags first.
	valueFlags := map[string]bool{"--url": true, "--token": true, "--checkpoint": true, "--doc": true, "--memory": true, "--name": true, "-o": true}
	flags, positional := []string{}, []string{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if valueFlags[a] {
				i++
				if i >= len(args) {
					return fmt.Errorf("%s requires a value", a)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, a)
		}
	}
	if e := fs.Parse(append(flags, positional...)); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx := context.Background()
	ref := ""
	n := 0
	if *checkpoint != "" {
		ref = "checkpoint:" + *checkpoint
		n++
	}
	if *doc != "" {
		ref = "document:" + *doc
		n++
	}
	if *memory != "" {
		ref = "memory:" + *memory
		n++
	}
	if n > 1 {
		return errors.New("only one attachment reference may be specified")
	}
	switch args[0] {
	case "put":
		if fs.NArg() != 1 {
			return errors.New("attach put requires a file")
		}
		p := fs.Arg(0)
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if *name == "" {
			*name = filepath.Base(p)
		}
		contentType := attachmentMIME(*name)
		if contentType == "" {
			contentType = http.DetectContentType(b)
		}
		x, created, e := c.PutAttachment(ctx, "", *name, contentType, ref, b)
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"sha256": x.SHA256, "size_bytes": x.SizeBytes, "url": "/v1/attachments/" + x.SHA256, "created": created})
	case "get":
		if fs.NArg() != 1 {
			return errors.New("attach get requires sha")
		}
		x, r, e := c.GetAttachment(ctx, fs.Arg(0))
		if e != nil {
			return e
		}
		defer r.Close()
		if *output == "" {
			_, e = io.Copy(out, r)
			return e
		}
		f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(f, r)
		if e == nil {
			return printJSON(out, map[string]any{"sha256": x.SHA256, "output": *output})
		}
		return e
	case "list":
		xs, e := c.ListAttachments(ctx, ref, 100)
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"attachments": xs})
	case "usage":
		u, e := c.AttachmentUsage(ctx)
		if e != nil {
			return e
		}
		return printJSON(out, u)
	default:
		return errors.New("usage: attach put|get|list|usage")
	}
}

func attachmentMIME(name string) string {
	return map[string]string{".json": "application/json", ".md": "text/markdown", ".csv": "text/csv", ".ndjson": "application/x-ndjson", ".log": "text/x-log", ".txt": "text/plain", ".html": "text/html", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".pdf": "application/pdf"}[strings.ToLower(filepath.Ext(name))]
}

// r2usage intentionally does no object-store access. It is safe to run where
// credentials are absent and reports a skipped result in that case.
func r2UsageCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("r2usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	alert := fs.Bool("alert", false, "send threshold alert")
	if e := fs.Parse(args); e != nil {
		return e
	}
	token, account := os.Getenv("CF_API_TOKEN"), os.Getenv("CF_ACCOUNT_ID")
	if token == "" || account == "" {
		return printJSON(out, map[string]any{"skipped": true, "reason": "CF_API_TOKEN and CF_ACCOUNT_ID are required"})
	}
	query := `query($account:String!,$month:Time!){viewer{accounts(filter:{accountTag:$account}){r2StorageAdaptiveGroups(limit:1,orderBy:[datetime_DESC]){sum{payloadSize objectCount}} r2OperationsAdaptiveGroups(limit:100,filter:{datetime_geq:$month}){dimensions{actionType} sum{requests}}}}}`
	month := time.Now().UTC().Format("2006-01") + "-01"
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"account": account, "month": month}})
	req, e := http.NewRequest(http.MethodPost, "https://api.cloudflare.com/client/v4/graphql", strings.NewReader(string(payload)))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare GraphQL returned %s", resp.Status)
	}
	var result struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Storage []struct {
						Sum struct{ PayloadSize, ObjectCount int64 } `json:"sum"`
					} `json:"r2StorageAdaptiveGroups"`
					Operations []struct {
						Dimensions struct {
							ActionType string `json:"actionType"`
						} `json:"dimensions"`
						Sum struct {
							Requests int64 `json:"requests"`
						} `json:"sum"`
					} `json:"r2OperationsAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&result); e != nil {
		return e
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("Cloudflare GraphQL returned errors")
	}
	var bytesTotal, objects, classA, classB int64
	if len(result.Data.Viewer.Accounts) > 0 {
		a := result.Data.Viewer.Accounts[0]
		if len(a.Storage) > 0 {
			bytesTotal = a.Storage[0].Sum.PayloadSize
			objects = a.Storage[0].Sum.ObjectCount
		}
		for _, op := range a.Operations {
			if strings.Contains(strings.ToLower(op.Dimensions.ActionType), "class a") {
				classA += op.Sum.Requests
			} else if strings.Contains(strings.ToLower(op.Dimensions.ActionType), "class b") {
				classB += op.Sum.Requests
			}
		}
	}
	alertReason := r2AlertReason(bytesTotal, classA, classB)
	output := map[string]any{"storage_bytes": bytesTotal, "object_count": objects, "class_a_month": classA, "class_b_month": classB, "storage_ratio": float64(bytesTotal) / float64(10<<30), "alert": alertReason != "", "alert_reason": alertReason}
	// Compare the independent Cloudflare total with the local immutable-object
	// ledger when this CLI has normal handoffkeep HTTP credentials configured.
	if local := configuredClient(); local.URL != "" && local.Token != "" {
		if err := remote.CFAccessPairError(local.CFAccessClientID, local.CFAccessClientSecret); err != nil {
			return err
		}
		if local, err := local.AttachmentUsage(context.Background()); err == nil {
			output["local_bytes_total"] = local.TotalBytes
			base := bytesTotal
			if base == 0 {
				base = local.TotalBytes
			}
			diff := bytesTotal - local.TotalBytes
			if diff < 0 {
				diff = -diff
			}
			if base > 0 && diff*10 > base {
				alertReason = "R2 storage differs from local attachment ledger by more than 10%"
				output["alert"] = true
				output["alert_reason"] = alertReason
			}
		}
	}
	if *alert && alertReason != "" {
		line := "handoffkeep r2usage alert: " + alertReason
		if hook := os.Getenv("HK_ALERT_DISCORD_WEBHOOK"); hook != "" {
			b, _ := json.Marshal(map[string]string{"content": line})
			_, _ = http.Post(hook, "application/json", strings.NewReader(string(b)))
		} else {
			fmt.Fprintln(out, line)
		}
	}
	return printJSON(out, output)
}

func r2AlertReason(bytesTotal, classA, classB int64) string {
	const storageFree = int64(10 << 30)
	for _, x := range []struct {
		value, cap int64
		label      string
	}{
		{bytesTotal, storageFree, "R2 storage"}, {classA, 1000000, "R2 Class A operations"}, {classB, 10000000, "R2 Class B operations"},
	} {
		if x.value*100 >= 90*x.cap {
			return x.label + " are at least 90% of free tier"
		}
		if x.value*100 >= 70*x.cap {
			return x.label + " are at least 70% of free tier"
		}
	}
	return ""
}

func validListen(addr string, tailnet bool) error {
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		return e
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("listen address must be an IP address")
	}
	if tailnet {
		if ip.To4() == nil || ip.To4()[0] != 100 || ip.To4()[1] < 64 || ip.To4()[1] > 127 {
			return errors.New("--listen-tailnet accepts only 100.64.0.0/10")
		}
		return nil
	}
	if !ip.IsLoopback() {
		return errors.New("--listen accepts loopback only")
	}
	return nil
}

const (
	httpDrainBudget  = 10 * time.Second
	workerStopBudget = 5 * time.Second
)

type serverBinding struct {
	server   *http.Server
	listener net.Listener
}

type backgroundWorker interface {
	Run(context.Context)
}

type storeCloser interface {
	Close()
}

// chatRetentionWorker is the always-on retention job for operator chat. Every
// run deletes at most store.ChatPruneMaxDelete expired terminal-state rows
// per table so a single execution can never empty a table; non-terminal rows
// are never deleted regardless of age.
type chatRetentionWorker struct {
	store    *store.Store
	interval time.Duration
}

const chatRetentionInterval = 24 * time.Hour

func (w chatRetentionWorker) Run(ctx context.Context) {
	if w.store == nil {
		return
	}
	interval := w.interval
	if interval <= 0 {
		interval = chatRetentionInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		deleted, err := w.store.PruneChat(ctx, store.ChatPruneMaxDelete)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("chat retention prune error: %v", err)
		} else if deleted > 0 {
			log.Printf("chat retention pruned %d rows", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type serveOptions struct {
	bindings         []serverBinding
	workers          []backgroundWorker
	store            storeCloser
	httpDrainBudget  time.Duration
	workerStopBudget time.Duration
}

// runServer owns the serving lifecycle. HTTP listeners stop accepting new
// connections first, active requests receive their drain budget, background
// workers are canceled only after that drain, and the store closes last.
func runServer(ctx context.Context, opts serveOptions) error {
	if len(opts.bindings) == 0 {
		return errors.New("at least one server binding is required")
	}
	if opts.httpDrainBudget <= 0 {
		opts.httpDrainBudget = httpDrainBudget
	}
	if opts.workerStopBudget <= 0 {
		opts.workerStopBudget = workerStopBudget
	}

	signalCtx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorkers()

	var workerWG sync.WaitGroup
	for _, worker := range opts.workers {
		if worker == nil {
			continue
		}
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			worker.Run(workerCtx)
		}()
	}

	serveErrs := make(chan error, len(opts.bindings))
	for _, binding := range opts.bindings {
		if binding.server == nil || binding.listener == nil {
			cancelWorkers()
			return errors.New("server and listener are required")
		}
		go func(binding serverBinding) {
			err := binding.server.Serve(binding.listener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serveErrs <- err
		}(binding)
	}

	var triggerErr error
	select {
	case <-signalCtx.Done():
	case triggerErr = <-serveErrs:
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), opts.httpDrainBudget)
	var drainWG sync.WaitGroup
	drainErrs := make(chan error, len(opts.bindings))
	for _, binding := range opts.bindings {
		drainWG.Add(1)
		go func(server *http.Server) {
			defer drainWG.Done()
			if err := server.Shutdown(drainCtx); err != nil {
				drainErrs <- err
			}
		}(binding.server)
	}
	drainWG.Wait()
	cancelDrain()
	close(drainErrs)
	var lifecycleErr error
	for err := range drainErrs {
		lifecycleErr = errors.Join(lifecycleErr, err)
	}
	if lifecycleErr != nil {
		// Shutdown already closed listeners. Close also cancels request contexts
		// so a handler honoring its context cannot outlive the bounded drain.
		for _, binding := range opts.bindings {
			_ = binding.server.Close()
		}
	}

	cancelWorkers()
	workersDone := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(workersDone)
	}()
	workerCtxTimeout, cancelWorkerWait := context.WithTimeout(context.Background(), opts.workerStopBudget)
	select {
	case <-workersDone:
	case <-workerCtxTimeout.Done():
		lifecycleErr = errors.Join(lifecycleErr, errors.New("background workers did not stop within budget"))
	}
	cancelWorkerWait()

	if opts.store != nil {
		opts.store.Close()
	}
	return errors.Join(triggerErr, lifecycleErr)
}

func serve(args []string, errout io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errout)
	listen := fs.String("listen", "127.0.0.1:8800", "loopback address")
	tail := fs.String("listen-tailnet", "", "optional tailnet address")
	stalledHours := fs.Int("started-stalled-hours", store.DefaultStartedStalledHours, "flag a started task stalled after this many hours without activity")
	iceboxDays := fs.Int("backlog-icebox-days", 0, "park backlog tasks idle this many days in icebox; 0 keeps the sweep off")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if e := validAutomationFlags(*stalledHours, *iceboxDays); e != nil {
		return e
	}
	if e := validListen(*listen, false); e != nil {
		return e
	}
	if *tail != "" {
		if e := validListen(*tail, true); e != nil {
			return e
		}
	}
	dbURL, auth := os.Getenv("HANDOFFKEEP_DB_URL"), os.Getenv("HANDOFFKEEP_AUTH_FILE")
	if dbURL == "" || auth == "" {
		return errors.New("HANDOFFKEEP_DB_URL and HANDOFFKEEP_AUTH_FILE are required")
	}
	retention, err := syncRetentionFromEnv()
	if err != nil {
		return err
	}
	ctx := context.Background()
	monitor, err := telemetry.FromEnv(buildVersion().Rev)
	if err != nil {
		return err
	}
	defer monitor.Close()
	mode := os.Getenv("WRK_RECORDS_API")
	var policy *wrkrecords.Policy
	var e error
	if mode != "" && mode != "off" {
		policy, e = wrkrecords.LoadPolicy(os.Getenv("WRK_RECORDS_POLICY_FILE"))
		if e != nil {
			return e
		}
	}
	st, e := store.OpenServer(ctx, dbURL, os.Getenv("HANDOFFKEEP_MIGRATE_DB_URL"), mode, policy, os.Getenv("WRK_RECORDS_STORE_GENERATION"), os.Getenv("WRK_RECORDS_RECOVERY_REQUIRED") == "true")
	if e != nil {
		return e
	}
	closeStore := true
	defer func() {
		if closeStore {
			st.Close()
		}
	}()
	if e := st.SetStartedStalledHours(*stalledHours); e != nil {
		return e
	}
	tokens, quotaAccounts, e := api.LoadTokens(auth)
	if e != nil {
		return e
	}
	importReadGrants, e := api.LoadImportReadGrants(auth, tokens)
	if e != nil {
		return e
	}
	hostGrants, e := api.LoadHostGrants(auth, tokens)
	if e != nil {
		return e
	}
	am, e := attachments.NewFromEnv(st)
	if e != nil {
		return e
	}
	youTrack, e := youTrackFromEnv(st)
	if e != nil {
		return e
	}
	svc := api.Service{Store: st, Attachments: am, YouTrack: youTrack, SearchBackend: searchBackendFromEnv()}
	workers := []backgroundWorker{chatRetentionWorker{store: st}, placementRetentionWorker{store: st}, syncRetentionWorker{store: st, retention: retention}}
	if *iceboxDays > 0 {
		workers = append(workers, backlogSweepWorker{store: st, days: *iceboxDays})
	}
	workers = append(workers, lastActivityRepairWorker{store: st})
	linear := linearFromEnv(st)
	if linear.boardWorker != nil {
		workers = append(workers, singletonWorker{store: st, id: store.LinearBoardWorkerLock, worker: linear.boardWorker})
	}
	if linear.opsWorker != nil {
		workers = append(workers, linear.opsWorker)
	}
	if forwarder := notificationForwarderFromEnv(st); forwarder != nil {
		workers = append(workers, singletonWorker{store: st, id: store.HubForwardWorkerLock, worker: forwarder})
	}
	notifier, e := decisionNotifierFromEnv(st)
	if n, ok := notifier.(api.DecisionNotifier); ok && e == nil {
		st.SetConsoleURL(n.ConsoleURL)
	}
	if e != nil {
		return e
	}
	workers = append(workers, notifier)
	uiHandler, e := uiFromEnv(st, youTrack)
	if e != nil {
		return e
	}
	mcpAccess, e := mcpAccessFromEnv()
	if e != nil {
		return e
	}
	handler := serverHandler(api.Server{Service: svc, Tokens: tokens, QuotaAccounts: quotaAccounts, ImportReadGrants: importReadGrants, HostGrants: hostGrants, UI: uiHandler, LinearWebhook: linear.webhook, LinearDecisions: linear.decisions, ClientRevisions: api.NewClientRevisions()}, monitor, mcpAccess)
	addresses := []string{*listen}
	if *tail != "" {
		addresses = append(addresses, *tail)
	}
	bindings := make([]serverBinding, 0, len(addresses))
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, binding := range bindings {
				_ = binding.listener.Close()
			}
			return err
		}
		bindings = append(bindings, serverBinding{
			server:   &http.Server{Addr: address, Handler: handler},
			listener: listener,
		})
	}
	closeStore = false
	return runServer(ctx, serveOptions{bindings: bindings, workers: workers, store: st})
}

func serverHandler(server api.Server, monitor *telemetry.Monitor, mcpAccess *cfaccess.Verifier) http.Handler {
	mux := http.NewServeMux()
	mcp := hkmcp.HTTPHandler(server.Service, server.Tokens, mcpAccess)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client, ok := server.Tokens.Client(r); ok && server.ImportReadGrants[client] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"import_read_only"}`+"\n")
			return
		}
		mcp.ServeHTTP(w, r)
	}))
	mux.Handle("/", server.Handler())
	return monitor.Handler(mux)
}

// mcpAccessFromEnv configures the second credential on /mcp. Incomplete
// configuration leaves that credential off, the same rule uiFromEnv follows;
// the bearer token path is unchanged. Configuration values are never logged.
func mcpAccessFromEnv() (*cfaccess.Verifier, error) {
	team := strings.TrimSpace(os.Getenv("HANDOFFKEEP_MCP_CF_TEAM_DOMAIN"))
	aud := strings.TrimSpace(os.Getenv("HANDOFFKEEP_MCP_CF_AUD"))
	emails := strings.TrimSpace(os.Getenv("HANDOFFKEEP_MCP_ALLOWED_EMAILS"))
	if team == "" || aud == "" || emails == "" {
		log.Printf("mcp.access disabled reason=incomplete_config team_domain_set=%t aud_set=%t allowlist_set=%t", team != "", aud != "", emails != "")
		return nil, nil
	}
	access, err := cfaccess.New(cfaccess.Config{
		TeamDomain:    team,
		AUD:           aud,
		AllowedEmails: strings.Split(emails, ","),
	})
	if err != nil {
		return nil, err
	}
	log.Printf("mcp.access enabled")
	return access, nil
}

// uiFromEnv intentionally treats incomplete Cloudflare Access configuration as
// disabled. The API remains available, but api.Server does not register /ui.
func uiFromEnv(st *store.Store, youTrack *youtrack.Service) (http.Handler, error) {
	team := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_CF_TEAM_DOMAIN"))
	aud := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_CF_AUD"))
	emails := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_ALLOWED_EMAILS"))
	if team == "" || aud == "" || emails == "" {
		return nil, nil
	}
	access, err := cfaccess.New(cfaccess.Config{
		TeamDomain:          team,
		AUD:                 aud,
		AllowedEmails:       strings.Split(emails, ","),
		AllowedServiceNames: strings.Split(strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES")), ","),
	})
	if err != nil {
		return nil, err
	}
	return ui.New(ui.Config{
		Store:    st,
		Access:   access,
		HubURL:   strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_URL")),
		HubToken: strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_TOKEN")),
		YouTrack: youTrack,
	})
}

// linearServer is what the Linear configuration turns on. Any field may be nil.
type linearServer struct {
	webhook     http.Handler
	decisions   *api.LinearDecisions
	opsWorker   backgroundWorker
	boardWorker backgroundWorker
}

// linearFromEnv follows uiFromEnv: incomplete configuration disables the route
// and leaves the rest of the server running. Linear's Comment payload has no
// labels, so the webhook cannot work on the secret alone. Decisions need the
// webhook, since that is how answers arrive, plus the operator's user id.
func linearFromEnv(st *store.Store) linearServer {
	secret := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET"))
	key := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_API_KEY"))
	clientID := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_CLIENT_SECRET"))
	var app *api.LinearAppToken
	if (clientID == "") != (clientSecret == "") {
		// Half a pair would fall back to the personal key silently.
		log.Printf("linear_oauth.disabled reason=incomplete_config")
	}
	if clientID != "" && clientSecret != "" {
		// An OAuth application wins over a personal key: its writes show as the
		// app, so nothing hk posts can be read as the operator's answer.
		app = &api.LinearAppToken{ClientID: clientID, ClientSecret: clientSecret}
		key = ""
	}
	// Either credential lets hk read Linear; the app is carried on its own
	// field, never as a placeholder in the key slot.
	credential := key != "" || app != nil
	operator := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_OPERATOR_USER_ID"))
	capacityPolicy, capacityErr := linearCapacityPolicyFromEnv()
	if capacityErr != nil {
		log.Printf("linear_capacity.disabled reason=invalid_policy err=%q", capacityErr)
	}
	linearAPI := api.LinearAPI{Key: key, App: app, RateGate: &api.LinearRateGate{}, Admission: st, CapacityPolicy: capacityPolicy, CapacityScopeVerified: capacityErr == nil && strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_CAPACITY_SCOPE")) == "workspace"}
	out := linearServer{boardWorker: linearBoardFromEnv(st, linearAPI)}
	if secret == "" || !credential {
		if secret != "" || operator != "" {
			log.Printf("linear_webhook.disabled reason=incomplete_config")
		}
		return out
	}
	webhook := api.LinearWebhook{Secret: secret, Issues: linearAPI, Store: st}
	if operator == "" {
		out.webhook = webhook
		return out
	}
	states, err := api.ParseLinearDecisionStates(os.Getenv("HANDOFFKEEP_LINEAR_DECISION_STATES"))
	if err != nil {
		// Comments still answer; only moving an issue to a state stops counting.
		log.Printf("linear_decisions.state_mapping_ignored err=%q", err.Error())
		states = nil
	}
	webhook.Decisions, webhook.Operator, webhook.StateOptions = st, operator, states
	team := strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_DECISION_TEAM"))
	if team == "" {
		team = "ROB"
	}
	out.webhook = webhook
	out.decisions = &api.LinearDecisions{Team: team, Linear: linearAPI}
	out.opsWorker = api.LinearDecisionOpsWorker{Store: st, Linear: linearAPI, RateGate: linearAPI.RateGate, Discord: discordDecisionsFromEnv()}
	return out
}

// discordDecisionsFromEnv returns nil when the channel is not configured, which
// leaves the Linear flow working without channel posts.
func discordDecisionsFromEnv() *api.DiscordWebhook {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL"))
	if raw == "" {
		return nil
	}
	webhook, err := api.ParseDiscordWebhookURL(raw)
	if err != nil {
		log.Printf("linear_decisions.discord_disabled err=%q", err.Error())
		return nil
	}
	return &api.DiscordWebhook{URL: webhook}
}

// decisionNotifierFromEnv always runs the decision request messages: without
// the webhook, what falls due settles unsent. HANDOFFKEEP_CONSOLE_URL and
// HANDOFFKEEP_DISCORD_OPERATOR_USER_ID are optional, but an invalid value of
// either stops the server, so a typo cannot silently drop every link or
// mention.
func decisionNotifierFromEnv(st *store.Store) (backgroundWorker, error) {
	console := ""
	if raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_CONSOLE_URL")); raw != "" {
		parsed, err := api.ParseConsoleURL(raw)
		if err != nil {
			return nil, err
		}
		console = parsed
	}
	operator := ""
	if raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID")); raw != "" {
		parsed, err := api.ParseDiscordOperatorUserID(raw)
		if err != nil {
			return nil, err
		}
		operator = parsed
	}
	return api.DecisionNotifier{Store: st, Discord: discordDecisionsFromEnv(), ConsoleURL: console, OperatorUserID: operator}, nil
}

// notificationForwarderFromEnv is opt-in, separately from the hub settings the
// console already uses: turning it on sends every row still unsent, however old.
func notificationForwarderFromEnv(st *store.Store) backgroundWorker {
	if strings.TrimSpace(os.Getenv("HANDOFFKEEP_NOTIFICATION_FORWARD")) != "on" {
		return nil
	}
	url := strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_URL"))
	token := strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_TOKEN"))
	if url == "" || token == "" {
		log.Printf("notification_forward.disabled reason=no_hub")
		return nil
	}
	return &hubforward.Forwarder{Store: st, URL: url, Token: token}
}

func runMCP() error {
	c := configuredClient()
	if c.URL == "" || c.Token == "" {
		return errors.New("mcp requires HANDOFFKEEP_URL and HANDOFFKEEP_TOKEN")
	}
	if err := remote.CFAccessPairError(c.CFAccessClientID, c.CFAccessClientSecret); err != nil {
		return err
	}
	return hkmcp.NewStdio(c, "stdio").Run(context.Background(), &mcp.StdioTransport{})
}

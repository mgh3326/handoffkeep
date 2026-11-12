package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// Documented exit codes. A Dagu job reads these; the JSON report is on stdout.
const (
	youtrackExitOK       = 0
	youtrackExitFailed   = 1
	youtrackExitConfig   = 2
	youtrackExitLockHeld = 3
	// youtrackExitProtocolOwned: a legacy push refused because the write
	// protocol owns YouTrack writes (hk 3907). Nothing was written.
	youtrackExitProtocolOwned = 5
	// youtrackExitFieldsOff: fields-backfill refused because
	// HANDOFFKEEP_YT_FIELDS_MODE is not on (hk 4022). Nothing was written.
	youtrackExitFieldsOff = 6
)

func youtrackCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackUsage)
	}
	switch args[0] {
	case "sync":
		if err := youtrackRefuseLegacyPush(args[0]); err != nil {
			return err
		}
		return youtrackSyncCmd(args[1:], out)
	case "adopt":
		if err := youtrackRefuseLegacyPush(args[0]); err != nil {
			return err
		}
		return youtrackAdoptCmd(args[1:], out)
	case "projects":
		return youtrackProjectsCmd(args[1:], out)
	case "ops":
		return youtrackOpsCmd(args[1:], out)
	case "canary":
		return youtrackCanaryCmd(args[1:], out)
	case "schema":
		return youtrackSchemaCmd(args[1:], out)
	case "comments-preflight":
		return youtrackCommentsCmd(args[1:], out, false)
	case "comments-backfill":
		return youtrackCommentsCmd(args[1:], out, true)
	case "issues-backfill":
		return youtrackIssuesBackfillCmd(args[1:], out)
	case "fields-backfill":
		return youtrackFieldsBackfillCmd(args[1:], out)
	case "audit":
		return youtrackAuditCmd(args[1:], out)
	default:
		return errors.New(youtrackUsage)
	}
}

// youtrackUsage also answers `yt`, the short name of this command.
const youtrackUsage = "usage: youtrack sync [--dry-run] | youtrack adopt --state <yt_state.json> [--dry-run] | youtrack projects list|put | youtrack ops [--state S] [--kind K] [--task N] | youtrack canary add|remove|list | youtrack schema plan|apply [--project P] | youtrack comments-preflight [--project P] [--days N] [--all] | youtrack comments-backfill [--project P] [--days N] [--all] [--by NAME] | youtrack audit [--project P] [--task N] [--enqueue] [--limit N] | youtrack issues-backfill [--project P] [--dry-run] | youtrack fields-backfill [--project P] [--task N] [--limit N] [--dry-run]"

func youtrackSyncCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print planned actions without writing hk or YouTrack")
	pull := fs.Bool("pull", false, "apply YouTrack activities to hk (first run only records the current position)")
	limit := fs.Int("limit", 50, "maximum tasks pushed in this run")
	activities := fs.Int("activities", 100, "maximum activities read in this run")
	comments := fs.Int("comments", 50, "maximum comments pushed per task")
	refresh := fs.Int("refresh", 100, "maximum mirrored tasks whose field cache is refreshed")
	reverts := fs.Int("reverts", 20, "maximum pending YouTrack reverts retried")
	outbox := fs.Int("outbox", 100, "maximum priority/lane outbox entries drained")
	docs := fs.Int("docs", 20, "maximum hk documents mirrored to YouTrack 기술 자료 in this run")
	ordered, err := splitArgs(args, "limit", "activities", "comments", "refresh", "reverts", "outbox", "docs")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("youtrack sync takes no positional arguments")
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	lock, acquired, err := st.TryYouTrackSyncLock(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		return exitCodeError{code: youtrackExitLockHeld, err: errors.New("another youtrack sync is running")}
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("youtrack.unlock_failed err=%q", err)
		}
	}()
	service := youtrack.NewService(st, client, youtrackServiceOptions())
	report, err := service.Sync(ctx, youtrack.SyncOptions{
		DryRun:        *dryRun,
		Pull:          *pull,
		TaskLimit:     *limit,
		ActivityLimit: *activities,
		CommentLimit:  *comments,
		RefreshLimit:  *refresh,
		RevertLimit:   *reverts,
		OutboxLimit:   *outbox,
		DocLimit:      *docs,
	})
	if printErr := printJSON(out, report); printErr != nil {
		return printErr
	}
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: err}
	}
	if len(report.Errors) > 0 {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d sync errors", len(report.Errors))}
	}
	return nil
}

// youtrackAdoptCmd maps the issues the Python mirror created onto the Go
// sync's rows, and marks the comments it already posted. It runs once, before
// the first sync, under the same lock a sync takes.
func youtrackAdoptCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack adopt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print the mapping without writing hk or YouTrack")
	statePath := fs.String("state", "", "the Python mirror's yt_state.json")
	ordered, err := splitArgs(args, "state")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 || *statePath == "" {
		return errors.New("usage: youtrack adopt --state <yt_state.json> [--dry-run]")
	}
	raw, err := os.ReadFile(*statePath)
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("read state: %w", err)}
	}
	var state youtrack.AdoptState
	if err := json.Unmarshal(raw, &state); err != nil {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("parse state: %w", err)}
	}
	// json.Unmarshal ignores unknown keys, so a wrong file decodes to empty maps.
	// Adopting nothing would make the next sync create a duplicate issue for
	// every task.
	if len(state.Issues) == 0 && len(state.Decisions) == 0 {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("state has no issues or decisions to adopt; check that --state points at the Python mirror's yt_state.json")}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	lock, acquired, err := st.TryYouTrackSyncLock(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		return exitCodeError{code: youtrackExitLockHeld, err: errors.New("another youtrack sync is running")}
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("youtrack.unlock_failed err=%q", err)
		}
	}()
	service := youtrack.NewService(st, client, youtrackServiceOptions())
	report, err := service.Adopt(ctx, state, *dryRun)
	if printErr := printJSON(out, report); printErr != nil {
		return printErr
	}
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: err}
	}
	if len(report.Errors) > 0 {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d adoption errors", len(report.Errors))}
	}
	return nil
}

// youtrackProtocolOwner names the env setting that hands YouTrack writes to
// the write protocol, or returns "" when none is set: comments mode canary or
// on, issues mode on, fields mode on, or document-comment mode on. An
// unparsable mode is an error, so the guard fails closed.
func youtrackProtocolOwner() (string, error) {
	comments, err := ytCommentsModeFromEnv()
	if err != nil {
		return "", err
	}
	if comments != store.YTCommentsOff {
		return "HANDOFFKEEP_YT_COMMENTS_MODE=" + string(comments), nil
	}
	issues, err := ytIssuesModeFromEnv()
	if err != nil {
		return "", err
	}
	if issues == store.YTIssuesOn {
		return "HANDOFFKEEP_YT_ISSUES_MODE=on", nil
	}
	fields, err := ytFieldsModeFromEnv()
	if err != nil {
		return "", err
	}
	if fields == store.YTFieldsOn {
		return "HANDOFFKEEP_YT_FIELDS_MODE=on", nil
	}
	docs, err := ytDocCommentsModeFromEnv()
	if err != nil {
		return "", err
	}
	if docs == store.YTDocCommentsOn {
		return "HANDOFFKEEP_YT_DOC_COMMENTS_MODE=on", nil
	}
	return "", nil
}

// youtrackRefuseLegacyPush guards the subcommands that push task or comment
// content to YouTrack outside the write protocol: sync (with or without
// --dry-run) and adopt (hk 3907). It runs before any argument, store or
// YouTrack use. The error's single stderr line names the mode that blocks the
// command.
func youtrackRefuseLegacyPush(sub string) error {
	owner, err := youtrackProtocolOwner()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if owner == "" {
		return nil
	}
	return exitCodeError{code: youtrackExitProtocolOwned, err: fmt.Errorf("youtrack %s refused: %s; the write protocol owns YouTrack writes (hk 3907)", sub, owner)}
}

// ytReadthroughModeFromEnv reads HANDOFFKEEP_YT_READTHROUGH_MODE: unset or
// "off" (the default) or "on", trimmed and case-sensitive like
// HANDOFFKEEP_YT_COMMENTS_MODE. Any other value is an error, so a typo fails
// closed instead of guessing a mode.
func ytReadthroughModeFromEnv() (bool, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_READTHROUGH_MODE")); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("HANDOFFKEEP_YT_READTHROUGH_MODE=%q: want off or on", raw)
	}
}

// youTrackFromEnv builds serve's legacy read/write-through service: the
// priority/lane overlay and cache seeding, YouTrack search, and the lane and
// priority export enqueue. It is on only with HANDOFFKEEP_YT_READTHROUGH_MODE=on
// (hk 3762): YOUTRACK_URL and YOUTRACK_BOT_API_KEY alone no longer turn it on,
// because the b1a comment worker needs them too and reads them itself. Off
// builds no client, so it never validates the URL; on without both keys is a
// startup error. Sync and pull build their own service and ignore the mode.
func youTrackFromEnv(st *store.Store) (*youtrack.Service, error) {
	on, err := ytReadthroughModeFromEnv()
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if !on {
		if baseURL != "" || token != "" {
			log.Printf("youtrack.readthrough disabled reason=mode_off")
		}
		return nil, nil
	}
	if baseURL == "" || token == "" {
		return nil, errors.New("HANDOFFKEEP_YT_READTHROUGH_MODE=on needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY")
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return nil, err
	}
	return youtrack.NewService(st, client, youtrackServiceOptions()), nil
}

// serveYouTrack is what serve wires from the YouTrack configuration.
type serveYouTrack struct {
	// readThrough backs api.Service.YouTrack and the console; nil disables it.
	readThrough *youtrack.Service
	// searchBackend is api.Service.SearchBackend.
	searchBackend string
	// comments is the b1a comment worker (yt_comments.go); nil when its mode
	// is off.
	comments backgroundWorker
}

// serveYouTrackFromEnv builds serve's YouTrack pieces from the environment.
// The search selector never turns read-through on: YouTrack search needs both
// HANDOFFKEEP_YT_READTHROUGH_MODE=on and HANDOFFKEEP_SEARCH_BACKEND=youtrack.
// The comment worker reads its own credentials and never depends on
// read-through.
func serveYouTrackFromEnv(st *store.Store) (serveYouTrack, error) {
	readThrough, err := youTrackFromEnv(st)
	if err != nil {
		return serveYouTrack{}, err
	}
	searchBackend := searchBackendFromEnv()
	if searchBackend == "youtrack" && readThrough == nil {
		log.Printf("search.backend youtrack ignored reason=readthrough_off")
		searchBackend = ""
	}
	comments, err := ytCommentsWorkerFromEnv(st)
	if err != nil {
		return serveYouTrack{}, err
	}
	return serveYouTrack{readThrough: readThrough, searchBackend: searchBackend, comments: comments}, nil
}

// youtrackProjectsCmd keeps the hk ↔ YouTrack project mapping. put resolves
// the short name to the project id once and stores both; list reads PG only.
func youtrackProjectsCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: youtrack projects list|put <hk project> <youtrack short name>")
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: youtrack projects list")
		}
		projects, err := st.ListYouTrackProjects(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"projects": projects})
	case "put":
		if len(args) != 3 {
			return errors.New("usage: youtrack projects put <hk project> <youtrack short name>")
		}
		baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
		token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
		if baseURL == "" || token == "" {
			return exitCodeError{code: youtrackExitConfig, err: errors.New("YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
		}
		client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
		if err != nil {
			return exitCodeError{code: youtrackExitConfig, err: err}
		}
		id, err := client.ProjectID(ctx, args[2])
		if err != nil {
			return err
		}
		project := youtrackProjectRow(args[1], id, args[2])
		if err := st.PutYouTrackProject(ctx, project); err != nil {
			return err
		}
		return printJSON(out, project)
	default:
		return errors.New("usage: youtrack projects list|put <hk project> <youtrack short name>")
	}
}

func youtrackProjectRow(hkProject, id, shortName string) store.YouTrackProject {
	return store.YouTrackProject{HKProject: hkProject, ProjectID: id, ShortName: shortName}
}

func youtrackServiceOptions() youtrack.Options {
	return youtrack.Options{
		Schema:           youtrack.DefaultSchema(),
		DefaultHKProject: strings.TrimSpace(os.Getenv("YOUTRACK_DEFAULT_HK_PROJECT")),
		DefaultLane:      strings.TrimSpace(os.Getenv("YOUTRACK_DEFAULT_LANE")),
		ConsoleBaseURL:   strings.TrimSpace(os.Getenv("YOUTRACK_CONSOLE_URL")),
		SearchTimeout:    youtrackSearchTimeoutFromEnv(),
	}
}

// youtrackSearchTimeoutFromEnv reads HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT, a Go
// duration such as "3s". Unset or invalid means the service default.
func youtrackSearchTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("search.youtrack_timeout_invalid value=%q using=default", raw)
		return 0
	}
	return d
}

// searchBackendFromEnv reads HANDOFFKEEP_SEARCH_BACKEND. Only "youtrack"
// changes the answer; an unknown value is logged and means PostgreSQL.
func searchBackendFromEnv() string {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SEARCH_BACKEND"))
	switch raw {
	case "", "postgres":
		return ""
	case "youtrack":
		return raw
	}
	log.Printf("search.backend_unknown value=%q using=postgres", raw)
	return ""
}

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
)

func youtrackCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: youtrack sync [--dry-run] [--limit N] [--activities N]")
	}
	switch args[0] {
	case "sync":
		return youtrackSyncCmd(args[1:], out)
	case "adopt":
		return youtrackAdoptCmd(args[1:], out)
	case "projects":
		return youtrackProjectsCmd(args[1:], out)
	default:
		return errors.New("usage: youtrack sync [--dry-run] | youtrack adopt --state <yt_state.json> [--dry-run] | youtrack projects list|put")
	}
}

func youtrackSyncCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print planned actions without writing hk or YouTrack")
	limit := fs.Int("limit", 50, "maximum tasks pushed in this run")
	activities := fs.Int("activities", 100, "maximum activities read in this run")
	comments := fs.Int("comments", 50, "maximum comments pushed per task")
	refresh := fs.Int("refresh", 100, "maximum mirrored tasks whose field cache is refreshed")
	reverts := fs.Int("reverts", 20, "maximum pending YouTrack reverts retried")
	outbox := fs.Int("outbox", 100, "maximum priority/lane outbox entries drained")
	ordered, err := splitArgs(args, "limit", "activities", "comments", "refresh", "reverts", "outbox")
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
		TaskLimit:     *limit,
		ActivityLimit: *activities,
		CommentLimit:  *comments,
		RefreshLimit:  *refresh,
		RevertLimit:   *reverts,
		OutboxLimit:   *outbox,
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

// youTrackFromEnv builds the read/write-through service the server uses.
// Incomplete configuration disables the feature rather than failing startup,
// following uiFromEnv and linearFromEnv.
func youTrackFromEnv(st *store.Store) (*youtrack.Service, error) {
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" && token == "" {
		return nil, nil
	}
	if baseURL == "" || token == "" {
		log.Printf("youtrack.disabled reason=incomplete_config")
		return nil, nil
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return nil, err
	}
	return youtrack.NewService(st, client, youtrackServiceOptions()), nil
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
	}
}

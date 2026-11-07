package main

import (
	"context"
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
	default:
		return errors.New("usage: youtrack sync [--dry-run] [--limit N] [--activities N]")
	}
}

func youtrackSyncCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print planned actions without writing hk or YouTrack")
	limit := fs.Int("limit", 50, "maximum tasks pushed in this run")
	activities := fs.Int("activities", 100, "maximum activities read in this run")
	comments := fs.Int("comments", 50, "maximum comments pushed per task")
	refresh := fs.Int("refresh", 100, "maximum linked tasks whose field cache is refreshed")
	reverts := fs.Int("reverts", 20, "maximum pending YouTrack reverts retried")
	ordered, err := splitArgs(args, "limit", "activities", "comments", "refresh", "reverts")
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

func youtrackServiceOptions() youtrack.Options {
	schema := youtrack.DefaultSchema()
	if v := strings.TrimSpace(os.Getenv("YOUTRACK_PROJECT")); v != "" {
		schema.Project = v
	}
	return youtrack.Options{
		Schema:           schema,
		DefaultHKProject: strings.TrimSpace(os.Getenv("YOUTRACK_DEFAULT_HK_PROJECT")),
		DefaultLane:      strings.TrimSpace(os.Getenv("YOUTRACK_DEFAULT_LANE")),
		ConsoleBaseURL:   strings.TrimSpace(os.Getenv("YOUTRACK_CONSOLE_URL")),
	}
}

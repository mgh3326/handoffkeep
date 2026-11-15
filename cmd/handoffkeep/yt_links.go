package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// `youtrack links` (hk 4640, hk 4731, design §9 S5,
// docs/youtrack-write-protocol.md §Kind issue_links): the issue-link
// projection's operator surface.
//
//   - status asks the server (GET /v1/yt/links/status); it is read-only and the
//     mode is the server's.
//   - plan runs in the serving container like `fields-backfill`, against the
//     database and the YouTrack adapter directly: it is the report of what the
//     first sweep would write, it works with the mode off, and it writes
//     nothing.

const youtrackLinksUsage = "usage: youtrack links status [--task N] [--limit N] | " +
	"youtrack links plan [--task N] [--project P] [--limit N]"

func youtrackLinksCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackLinksUsage)
	}
	switch args[0] {
	case "status":
		return youtrackLinksStatusCmd(args[1:], out)
	case "plan":
		return youtrackLinksPlanCmd(args[1:], out)
	}
	return errors.New(youtrackLinksUsage)
}

func youtrackLinksStatusCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack links status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	task := fs.Int64("task", 0, "only this task's owned and unowned links and held ops (0: every task)")
	limit := fs.Int("limit", 100, "rows per list (owned, unowned, held)")
	ordered, err := splitArgs(args, "task", "limit", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 || *task < 0 || *limit < 1 || *limit > store.YTOpListMax {
		return errors.New(youtrackLinksUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := c.YTLinksStatus(ctx, *task, *limit)
	if err != nil {
		if *task > 0 && remote.ErrorCode(err) == "not_found" {
			return exitCodeError{code: youtrackExitNotFound, err: fmt.Errorf("no such task: %d", *task)}
		}
		return err
	}
	return printJSON(out, status)
}

const youtrackLinksPlanUsage = "usage: youtrack links plan [--task N] [--project P] [--limit N] " +
	"(--limit counts bound tasks; a plan writes nothing and works with the mode off)"

// youtrackLinksPlanCmd is `youtrack links plan` (hk 4731): the read-only report
// of what the first issue-link sweep would write. It opens the database
// read-only and reads every selected bound issue's links through the S1
// adapter (GET only), then prints the totals, one row per task with an add or
// a delete, and the number of YouTrack GETs it made. It neither needs nor
// changes HANDOFFKEEP_YT_LINKS_MODE, and it makes no request that is not a GET.
func youtrackLinksPlanCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack links plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	task := fs.Int64("task", 0, "only this task")
	limit := fs.Int("limit", 0, "at most N bound tasks (0: every bound task)")
	ordered, err := splitArgs(args, "project", "task", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackLinksPlanUsage)
	}
	if fs.NArg() != 0 || *task < 0 || *limit < 0 {
		return errors.New(youtrackLinksPlanUsage)
	}
	mode, err := ytLinksModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	remote := youtrack.NewWriteRemote(client)
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A read-only pool: a second barrier beside the store's read-only
	// transactions, so the report cannot write even by accident.
	st, err := store.OpenReadOnly(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// OpenReadOnly never migrates: an older schema must say so, not fail
	// with a missing relation halfway through the walk.
	if applied, err := st.MigrationApplied(openCtx, store.YTIssueLinksMigration); err != nil {
		return err
	} else if !applied {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first", store.YTIssueLinksMigration)}
	}
	st.SetYTLinksMode(mode, remote.Tenant())
	report, err := st.PlanYTLinks(context.Background(), store.YTLinksPlanOptions{
		Project: strings.TrimSpace(*project), TaskID: *task, Limit: *limit}, remote)
	if errors.Is(err, store.ErrTaskNotFound) {
		return exitCodeError{code: youtrackExitNotFound, err: fmt.Errorf("no such task: %d", *task)}
	}
	if err != nil {
		return err
	}
	report.Gets = remote.Gets()
	return printJSON(out, report)
}

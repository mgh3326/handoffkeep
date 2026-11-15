package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// `youtrack links status` (hk 4640, design §9 S5, docs/youtrack-write-protocol.md
// §Kind issue_links): the issue-link projection's state through the server
// (GET /v1/yt/links/status). Read-only; the mode is the server's.

const youtrackLinksUsage = "usage: youtrack links status [--task N] [--limit N]"

func youtrackLinksCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "status" {
		return errors.New(youtrackLinksUsage)
	}
	return youtrackLinksStatusCmd(args[1:], out)
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

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
)

const placementsListUsage = "usage: placements list [--since RFC3339] [--host H] [--job J] [--limit N]"

// placementsCmd dispatches the placements subcommands.
func placementsCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: placements list")
	}
	switch args[0] {
	case "list":
		return placementsListCmd(args[1:], out)
	default:
		return errors.New("usage: placements list")
	}
}

// placementsListCmd prints placement decisions as the server's JSON, newest
// first.
func placementsListCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("placements list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var f remote.PlacementFilter
	since := fs.String("since", "", "only decisions at or after this RFC3339 time (server default: last 180 days)")
	host := fs.String("host", "", "only decisions that chose this host")
	job := fs.String("job", "", "only decisions for this job")
	limit := fs.String("limit", "", "maximum results, 1-1000 (server default 100)")
	parsed, err := splitArgs(args, "since", "host", "job", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(parsed); err != nil || fs.NArg() != 0 {
		return errors.New(placementsListUsage)
	}
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return errors.New("placements list: --since must be RFC3339: " + placementsListUsage)
		}
		f.Since = t
	}
	if *limit != "" {
		n, err := strconv.Atoi(*limit)
		if err != nil || n < 1 || n > 1000 {
			return errors.New("placements list: --limit must be 1-1000: " + placementsListUsage)
		}
		f.Limit = n
	}
	f.Host, f.Job = *host, *job
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := c.ListPlacements(ctx, f)
	if err != nil {
		return err
	}
	return printJSON(out, list)
}

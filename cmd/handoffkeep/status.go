package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
)

const statusUsage = "usage: handoffkeep status --project P [--since RFC3339] [--json]"

func statusCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	project := fs.String("project", "", "project name")
	sinceText := fs.String("since", "", "RFC3339 time")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *project == "" {
		return errors.New(statusUsage)
	}
	var since time.Time
	if *sinceText != "" {
		var err error
		since, err = time.Parse(time.RFC3339, *sinceText)
		if err != nil {
			return errors.New(statusUsage)
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, err := c.ProjectFrontier(ctx, *project, since)
	if err != nil {
		if remote.ErrorCode(err) == "project_not_found" {
			return errors.New("project_not_found")
		}
		return err
	}
	if *asJSON {
		return printJSON(out, x)
	}
	suffix := ""
	if x.Truncated {
		suffix = " (truncated)"
	}
	fmt.Fprintf(out, "%s: %d landable · %d blocked · %d waiting · %d verifying%s\n", x.Project, x.Counts.Landable, x.Counts.Blocked, x.Counts.Waiting, x.Counts.Verifying, suffix)
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "since %s KST: ", x.Since.In(loc).Format("2006-01-02 15:04"))
	if x.ChangedSince.Count == 0 {
		fmt.Fprintln(out, "no change")
	} else {
		items := []string{}
		for i, v := range x.ChangedSince.Items {
			if i == 3 {
				break
			}
			parts := strings.Split(v.PR, "/")
			items = append(items, fmt.Sprintf("#%s %s", parts[len(parts)-1], v.What))
		}
		fmt.Fprintf(out, "%d changed (%s)\n", x.ChangedSince.Count, strings.Join(items, ", "))
	}
	fmt.Fprintf(out, "gates: %d decisions open · %d held · %d findings\n", x.Gates.DecisionsOpen, x.Gates.Held, x.Gates.Findings)
	return nil
}

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"strings"
	"time"
)

func jobsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "skills-report" {
		return errors.New("usage: jobs skills-report")
	}
	fs := flag.NewFlagSet("jobs skills-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	sinceArg := fs.String("since", "30d", "window")
	limit := fs.Int("limit", 100, "maximum jobs")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 1000 {
		return errors.New("invalid skills report arguments")
	}
	var since time.Time
	var err error
	if strings.HasSuffix(*sinceArg, "d") {
		var days int
		days, err = strconv.Atoi(strings.TrimSuffix(*sinceArg, "d"))
		if err == nil && days > 0 {
			since = time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
		} else {
			return errors.New("invalid since")
		}
	} else if d, e := time.ParseDuration(*sinceArg); e == nil && d > 0 {
		since = time.Now().UTC().Add(-d)
	} else {
		since, err = time.Parse(time.RFC3339, *sinceArg)
		if err != nil {
			return err
		}
	}
	if err = mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := c.JobSkillsReport(ctx, since, *limit)
	if err != nil {
		return err
	}
	return printJSON(out, report)
}

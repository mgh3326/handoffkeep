package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"
)

const lanesLookUsage = "usage: lanes look <lane> --notice <lane_check event_id> [--note <text>]"

// lanesCmd serves `lanes look <lane> --notice <event_id> [--note <text>]`
// (operator decision 418, hk 5201): the lane records that it looked at the
// state its lane_check notice named. The next check skips the lane until its
// findings change. The lane's last look, not the clock, is what stops a
// repeated notice.
func lanesCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "look" {
		return errors.New(lanesLookUsage)
	}
	fs := flag.NewFlagSet("lanes look", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	notice := fs.String("notice", "", "the lane_check notice event id the lane looked at")
	note := fs.String("note", "", "what the look found, in one line")
	ordered, err := splitArgs(args[1:], "notice", "note")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || *notice == "" {
		return errors.New(lanesLookUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.RecordLaneLook(ctx, fs.Arg(0), *notice, *note)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

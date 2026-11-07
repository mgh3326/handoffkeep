package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const commandsUsage = "usage: commands list --executor-lane <lane> [--state recorded|delivered|acked|done|failed|expired|superseded] [--after-id N] [--limit N] | commands receipt <id> --state acked|done|failed [--error-code C] [--note T]"

func commandsCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(commandsUsage)
	}
	fs := flag.NewFlagSet("commands "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	lane := fs.String("executor-lane", "", "executor lane")
	state := fs.String("state", "", "state filter, or the receipt state")
	errorCode := fs.String("error-code", "", "failure code for a failed receipt")
	note := fs.String("note", "", "executor note for the receipt")
	afterID := fs.Int64("after-id", 0, "exclusive id cursor")
	limit := fs.Int("limit", 100, "maximum rows")
	rest, err := splitArgs(args[1:], "executor-lane", "state", "error-code", "note", "after-id", "limit")
	if err != nil {
		return err
	}
	if err = fs.Parse(rest); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "list":
		if fs.NArg() != 0 || *lane == "" {
			return errors.New(commandsUsage)
		}
		if err := mustClient(c); err != nil {
			return err
		}
		xs, err := c.ListOperatorCommands(ctx, *lane, *state, *afterID, *limit)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"commands": xs})
	case "receipt":
		if fs.NArg() != 1 {
			return errors.New(commandsUsage)
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("command id must be positive")
		}
		if *state != "acked" && *state != "done" && *state != "failed" {
			return errors.New("--state must be acked, done or failed")
		}
		if *state == "failed" && *errorCode == "" {
			return errors.New("--error-code is required for a failed receipt")
		}
		if err := mustClient(c); err != nil {
			return err
		}
		x, err := c.OperatorCommandReceipt(ctx, id, store.OperatorCommandReceiptInput{State: *state, ErrorCode: *errorCode, Note: *note})
		if err != nil {
			return err
		}
		return printJSON(out, x)
	}
	return errors.New(commandsUsage)
}

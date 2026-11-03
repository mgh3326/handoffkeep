package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"
)

func linearCmd(args []string, out io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: handoffkeep linear metadata refresh | linear board reconcile --task ID")
	}
	metadata := args[0] == "metadata" && args[1] == "refresh"
	reconcile := args[0] == "board" && args[1] == "reconcile"
	reservation := args[0] == "reservations" && args[1] == "reconcile"
	if !metadata && !reconcile && !reservation {
		return errors.New("unknown linear command")
	}
	fs := flag.NewFlagSet("linear", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	client := remoteClient(fs)
	var issueID *string
	if reservation {
		issueID = fs.String("issue", "", "one stable issue UUID to repair")
	}
	var taskID *int64
	if reconcile {
		taskID = fs.Int64("task", 0, "one task whose issue webhook was missed")
	}
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || (reconcile && *taskID < 1) || (reservation && *issueID == "") {
		return errors.New("invalid linear command arguments")
	}
	if err := mustClient(client); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	status := "refreshed"
	if metadata {
		if err := client.RefreshLinearMetadata(ctx); err != nil {
			return err
		}
	} else {
		var err error
		if reconcile {
			status, err = client.ReconcileLinearBoard(ctx, *taskID)
		} else {
			status, err = client.ReconcileLinearReservation(ctx, *issueID)
		}
		if err != nil {
			return err
		}
	}
	return printJSON(out, map[string]string{"status": status})
}

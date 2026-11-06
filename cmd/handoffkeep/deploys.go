package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const servicesUsage = "usage: services put <name> --repo owner/name --pattern P... --mode manual|auto|none [--target T...] | services list | services pending <name> [--limit N]"
const deploysUsage = "usage: deploys start --file <json> | deploys finish <attempt_id> --state success|failed|rolled_back [--failed-step S]"

func servicesCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(servicesUsage)
	}
	fs := flag.NewFlagSet("services "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var patterns, targets refFlags
	repo := fs.String("repo", "", "owner/name")
	mode := fs.String("mode", "", "manual, auto or none")
	fs.Var(&patterns, "pattern", "path pattern (repeatable)")
	fs.Var(&targets, "target", "target (repeatable)")
	limit := fs.Int("limit", 100, "maximum changes")
	rest, err := splitArgs(args[1:], "repo", "mode", "pattern", "target", "limit")
	if err != nil {
		return err
	}
	if err = fs.Parse(rest); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch {
	case args[0] == "put" && fs.NArg() == 1:
		if err = mustClient(c); err != nil {
			return err
		}
		x, err := c.PutService(ctx, store.Service{Name: fs.Arg(0), Repo: *repo, PathPatterns: patterns, DeployMode: *mode, Targets: append([]string{}, targets...)})
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case args[0] == "list" && fs.NArg() == 0:
		if err = mustClient(c); err != nil {
			return err
		}
		xs, err := c.ListServices(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"services": xs})
	case args[0] == "pending" && fs.NArg() == 1 && *limit >= 1 && *limit <= 1000:
		if err = mustClient(c); err != nil {
			return err
		}
		xs, err := c.PendingChanges(ctx, fs.Arg(0), *limit)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"service": fs.Arg(0), "changes": xs})
	}
	return errors.New(servicesUsage)
}

func deploysCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(deploysUsage)
	}
	fs := flag.NewFlagSet("deploys "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	file := fs.String("file", "", "start body as JSON")
	state := fs.String("state", "", "finish state")
	failedStep := fs.String("failed-step", "", "failed step")
	rest, err := splitArgs(args[1:], "file", "state", "failed-step")
	if err != nil {
		return err
	}
	if err = fs.Parse(rest); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case args[0] == "start" && fs.NArg() == 0 && *file != "":
		raw, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		var in store.DeployStart
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&in); err != nil {
			return err
		}
		if _, err = dec.Token(); err != io.EOF {
			return errors.New("deploy file has data after the JSON object")
		}
		if err = mustClient(c); err != nil {
			return err
		}
		d, created, err := c.StartDeploy(ctx, in)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"created": created, "deploy": d})
	case args[0] == "finish" && fs.NArg() == 1 && *state != "":
		if err = mustClient(c); err != nil {
			return err
		}
		d, err := c.FinishDeploy(ctx, fs.Arg(0), store.DeployFinish{State: *state, FailedStep: *failedStep})
		if err != nil {
			return err
		}
		return printJSON(out, d)
	}
	return errors.New(deploysUsage)
}

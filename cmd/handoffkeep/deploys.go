package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const servicesUsage = "usage: services records-list | services records-put <name> --repo owner/name --prefix P... | services records-deactivate <name>"

func servicesCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(servicesUsage)
	}
	fs := flag.NewFlagSet("services "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var prefixes refFlags
	repo := fs.String("repo", "", "owner/name")
	fs.Var(&prefixes, "prefix", "record prefix (repeatable)")
	rest, err := splitArgs(args[1:], "repo", "prefix")
	if err != nil {
		return err
	}
	if err = fs.Parse(rest); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch {
	case args[0] == "records-put" && fs.NArg() == 1:
		if err = mustClient(c); err != nil {
			return err
		}
		x, err := c.PutDeployService(ctx, store.DeployService{Name: fs.Arg(0), Repo: *repo, RecordPrefixes: prefixes})
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case args[0] == "records-list" && fs.NArg() == 0:
		if err = mustClient(c); err != nil {
			return err
		}
		xs, err := c.ListDeployServices(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"services": xs})
	case args[0] == "records-deactivate" && fs.NArg() == 1:
		if err = mustClient(c); err != nil {
			return err
		}
		x, err := c.DeactivateDeployService(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return printJSON(out, x)
	}
	return errors.New(servicesUsage)
}

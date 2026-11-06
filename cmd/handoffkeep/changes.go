package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	changesUsage       = "usage: handoffkeep changes verify|show"
	changesVerifyUsage = "usage: handoffkeep changes verify --pr URL --head SHA --level L --verifier V --family F --evidence E [--job ID] [--profile P] [--model M] [--harness H] [--ci success|failure|pending|none] [--source direct_merge|mergewake]"
	changesShowUsage   = "usage: handoffkeep changes show --pr URL | --task N"
)

// changesCmd reads and writes the work record's changes.
func changesCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(changesUsage)
	}
	switch args[0] {
	case "verify":
		return changesVerifyCmd(args[1:], out)
	case "show":
		return changesShowCmd(args[1:], out)
	}
	return errors.New(changesUsage)
}

// changesVerifyCmd records the verdict for one head of a change (hk 3120)
// and prints the stored row.
func changesVerifyCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("changes verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	var in store.VerificationInput
	fs.StringVar(&in.PR, "pr", "", "PR URL")
	fs.StringVar(&in.HeadSHA, "head", "", "verified head SHA")
	fs.StringVar(&in.Level, "level", "", "live-verified, unit-test-verified, type-check-only, verifier-blocked or verifier-failed")
	fs.StringVar(&in.Verifier, "verifier", "", "who verified")
	fs.StringVar(&in.Family, "family", "", "the verifier's model family")
	fs.StringVar(&in.Evidence, "evidence", "", "https URL or hk:doc <key>")
	fs.StringVar(&in.VerifierJobID, "job", "", "the verifier's job id")
	fs.StringVar(&in.Profile, "profile", "", "the verifier's launch profile")
	fs.StringVar(&in.Model, "model", "", "the verifier's model")
	fs.StringVar(&in.Harness, "harness", "", "the verifier's harness")
	fs.StringVar(&in.CIResult, "ci", "", "success, failure, pending or none")
	fs.StringVar(&in.Source, "source", "direct_merge", "direct_merge or mergewake")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || in.PR == "" || in.HeadSHA == "" || in.Level == "" || in.Verifier == "" || in.Family == "" || in.Evidence == "" {
		return errors.New(changesVerifyUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, err := c.RecordVerification(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, v)
}

// changesShowCmd prints the changes for a PR or a task, with their
// verifications.
func changesShowCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("changes show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	pr := fs.String("pr", "", "PR URL")
	task := fs.Int64("task", 0, "task id")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*pr == "") == (*task == 0) || *task < 0 {
		return errors.New(changesShowUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	xs, err := c.ListChanges(ctx, *pr, *task)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"changes": xs})
}

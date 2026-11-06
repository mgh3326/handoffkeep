package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	changesUsage         = "usage: handoffkeep changes verify|show|checks|findings"
	changesVerifyUsage   = "usage: handoffkeep changes verify --pr URL --head SHA --level L --verifier V --family F --evidence E [--job ID] [--profile P] [--model M] [--harness H] [--ci success|failure|pending|none] [--source direct_merge|mergewake]"
	changesShowUsage     = "usage: handoffkeep changes show --pr URL | --task N | --repo OWNER/NAME --pr N [--all-heads] [--include checks,threads,findings,gate,reviews]"
	changesChecksUsage   = "usage: handoffkeep changes checks --repo OWNER/NAME --pr N [--all-heads]"
	changesFindingsUsage = "usage: handoffkeep changes findings --repo OWNER/NAME --pr N [--open]"
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
	case "checks":
		return changesChecksCmd(args[1:], out)
	case "findings":
		return changesFindingsCmd(args[1:], out)
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

// changesShowCmd prints the changes for a PR URL or a task, with their
// verifications; with --repo it prints one PR's state as mergewake wrote it
// (state, gate, checks, threads, findings and their freshness).
func changesShowCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("changes show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	pr := fs.String("pr", "", "PR URL, or the PR number with --repo")
	task := fs.Int64("task", 0, "task id")
	repo := fs.String("repo", "", "owner/name: read one PR's state")
	allHeads := fs.Bool("all-heads", false, "with --repo: include the checks of earlier heads")
	include := fs.String("include", "", "with --repo: comma list of checks, threads, findings, gate, reviews")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(changesShowUsage)
	}
	if *repo != "" {
		n, err := strconv.Atoi(*pr)
		if err != nil || n < 1 || *task != 0 {
			return errors.New(changesShowUsage)
		}
		return printChangeState(c, out, *repo, n, *allHeads, *include, nil)
	}
	if (*pr == "") == (*task == 0) || *task < 0 || *allHeads || *include != "" {
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

// printChangeState reads one PR's state and prints it; keep, when set,
// filters its findings first.
func printChangeState(c *remote.Client, out io.Writer, repo string, pr int, allHeads bool, include string, keep func(store.ChangeFinding) bool) error {
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, err := c.GetChange(ctx, repo, pr, allHeads, include)
	if err != nil {
		return err
	}
	if keep != nil && v.Findings != nil {
		kept := []store.ChangeFinding{}
		for _, f := range *v.Findings {
			if keep(f) {
				kept = append(kept, f)
			}
		}
		v.Findings = &kept
	}
	return printJSON(out, v)
}

// changesChecksCmd prints one PR's checks for its current head (every head
// with --all-heads) and the freshness of each section.
func changesChecksCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("changes checks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	repo := fs.String("repo", "", "owner/name")
	pr := fs.Int("pr", 0, "PR number")
	allHeads := fs.Bool("all-heads", false, "include the checks of earlier heads")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *repo == "" || *pr < 1 {
		return errors.New(changesChecksUsage)
	}
	return printChangeState(c, out, *repo, *pr, *allHeads, "checks", nil)
}

// changesFindingsCmd prints one PR's findings, open first; --open keeps only
// the open ones (a finding's state comes from its thread, or from whether its
// head is still the PR's head).
func changesFindingsCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("changes findings", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	repo := fs.String("repo", "", "owner/name")
	pr := fs.Int("pr", 0, "PR number")
	open := fs.Bool("open", false, "only open findings")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *repo == "" || *pr < 1 {
		return errors.New(changesFindingsUsage)
	}
	var keep func(store.ChangeFinding) bool
	if *open {
		keep = func(f store.ChangeFinding) bool { return f.Status == "open" }
	}
	return printChangeState(c, out, *repo, *pr, false, "findings", keep)
}

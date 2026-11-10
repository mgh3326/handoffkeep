package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

const youtrackSchemaUsage = "usage: youtrack schema plan|apply [--project P] [--json]"

// youtrackSchemaTimeout bounds a whole run: a full tenant is a few hundred
// requests, each waiting out a 429 when YouTrack asks.
const youtrackSchemaTimeout = 15 * time.Minute

// youtrackSchemaRetryAfter is the longest Retry-After the run waits for.
const youtrackSchemaRetryAfter = 60 * time.Second

// youtrackSchemaCmd brings the YouTrack tenant to the schema hk declares
// (hk 3836). plan lists what is missing and writes nothing; apply makes it so.
// Both run inside the serving container, like `youtrack canary`: the database
// URL and the YouTrack credentials come from the server env.
func youtrackSchemaCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "apply") {
		return errors.New(youtrackSchemaUsage)
	}
	fs := flag.NewFlagSet("youtrack schema", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "limit the run to one hk project")
	asJSON := fs.Bool("json", false, "print JSON instead of one tab-separated line per change")
	ordered, err := splitArgs(args[1:], "project")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackSchemaUsage)
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{
		BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-youtrack-schema",
		MaxRetries: 6, RetryAfterCap: youtrackSchemaRetryAfter,
	})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), youtrackSchemaTimeout)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	runner := &youtrack.SchemaRunner{Client: client, Store: st, Schema: youtrack.DefaultSchema()}
	return runYouTrackSchema(ctx, out, runner, args[0], *project, *asJSON)
}

// runYouTrackSchema plans, and for apply performs the plan. apply prints the
// changes it made, including the ones before a failure.
func runYouTrackSchema(ctx context.Context, out io.Writer, runner *youtrack.SchemaRunner, sub, project string, asJSON bool) error {
	plan, err := runner.Plan(ctx, project)
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: err}
	}
	var applyErr error
	if sub == "apply" {
		var applied int
		applied, applyErr = runner.Apply(ctx, plan)
		plan.Changes = plan.Changes[:applied]
	}
	if asJSON {
		raw, err := plan.JSON()
		if err != nil {
			return err
		}
		if _, err := out.Write(append(raw, '\n')); err != nil {
			return err
		}
	} else if _, err := io.WriteString(out, plan.Text()); err != nil {
		return err
	}
	if applyErr != nil {
		return exitCodeError{code: youtrackExitFailed, err: applyErr}
	}
	return nil
}

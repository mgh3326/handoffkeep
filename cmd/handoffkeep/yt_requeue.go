package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// `youtrack ops requeue` (hk 5506): returns the ops YouTrack's read-only
// maintenance answer rejected before hk 5494 to prepared, so the dispatcher
// sends them once more. It runs in the serving container with the server's
// environment, like the other youtrack subcommands, and opens the database
// directly (store.RequeueYTReadOnlyOps). Without --apply it reports what it
// would do and writes nothing.
const youtrackOpsRequeueUsage = "usage: youtrack ops requeue [--op N ...] [--after-id N] [--limit N] --by NAME [--apply]"

// int64List is a repeatable --op flag.
type int64List []int64

func (l *int64List) String() string { return fmt.Sprint([]int64(*l)) }

func (l *int64List) Set(v string) error {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 1 {
		return fmt.Errorf("op id must be positive: %q", v)
	}
	*l = append(*l, id)
	return nil
}

func youtrackOpsRequeueCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack ops requeue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ops int64List
	fs.Var(&ops, "op", "op id to consider (repeatable; default: the rejected ops after --after-id)")
	afterID := fs.Int64("after-id", 0, "rejected ops with id greater than this (when no --op is given)")
	limit := fs.Int("limit", store.YTRequeueListMax, "most ops one pass considers")
	by := fs.String("by", "", "operator who requeues the ops (required)")
	apply := fs.Bool("apply", false, "requeue the eligible ops (default: report only)")
	ordered, err := splitArgs(args, "op", "after-id", "limit", "by")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackOpsRequeueUsage)
	}
	bindBy := strings.TrimSpace(*by)
	if fs.NArg() != 0 || *afterID < 0 || *limit < 1 || *limit > store.YTRequeueListMax || bindBy == "" {
		return errors.New(youtrackOpsRequeueUsage)
	}
	if len(bindBy) > store.YTRequeueByMax {
		return fmt.Errorf("--by is longer than %d characters", store.YTRequeueByMax)
	}
	commentsMode, err := ytCommentsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	docsMode, err := ytDocsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	docCommentsMode, err := ytDocCommentsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	// The tenant the ops were prepared under: the serving process's own.
	tenant := youtrack.NewWriteRemote(client).Tenant()
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openStore := store.OpenReadOnly
	if *apply {
		openStore = store.Open
	}
	st, err := openStore(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if !*apply {
		applied, err := st.MigrationApplied(openCtx, store.YTWriteOpRequeueMigration)
		if err != nil {
			return err
		}
		if !applied {
			return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the preflight never migrates", store.YTWriteOpRequeueMigration)}
		}
	}
	// The admission the dispatcher applies (ytAdmitSQL) reads the modes the
	// server runs with, so the pass sets them as serve does. A document's
	// article gate needs the import's /shared and /unassigned projects, which
	// only YouTrack knows, so they are read here as serve reads them.
	st.SetYTCommentsMode(commentsMode)
	st.SetYTDocCommentsMode(docCommentsMode)
	if docsMode == store.YTDocsOn {
		bucketCtx, cancelBucket := context.WithTimeout(context.Background(), ytCommentsGetTimeout)
		buckets, err := ytArticleBuckets(bucketCtx, client)
		cancelBucket()
		if err != nil {
			return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("HANDOFFKEEP_YT_DOCS_MODE=on: read the article bucket projects: %w", err)}
		}
		projects := map[string]store.YTArticleProject{}
		for k, b := range buckets {
			projects[k] = store.YTArticleProject{ID: b.ID, ShortName: b.ShortName}
		}
		st.SetYTDocsMode(store.YTDocsOn, tenant, projects)
	}
	report, err := st.RequeueYTReadOnlyOps(context.Background(), time.Now().UTC(), store.YTRequeueOptions{
		OpIDs: ops, AfterID: *afterID, Limit: *limit, By: bindBy, Tenant: tenant, Apply: *apply})
	if err != nil {
		return err
	}
	return printJSON(out, report)
}

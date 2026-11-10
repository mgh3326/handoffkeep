package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Slice b1a (hk 3739): a new hk comment on a canary task also gets a
// protocol-tracked copy on its YouTrack issue. docs/youtrack-write-protocol.md.

const (
	// ytCommentsInterval is how often the worker runs one Drive pass.
	ytCommentsInterval = 5 * time.Second
	// ytCommentsDispatchTimeout bounds one YouTrack call (and one evidence
	// search); ytCommentsSettleTimeout bounds recording its outcome. When
	// serve stops, a call in flight finishes or times out and its settlement
	// gets at most ytCommentsSettleTimeout, so the worker returns within
	// their sum, under serve's worker stop budget (workerStopBudget, 5s), and
	// before the store closes. A call that times out becomes remote_unknown;
	// a settlement that cannot finish in time (a locked row, a slow database)
	// leaves the op dispatched, durable and unrecorded: the next worker's
	// stale recovery makes it remote_unknown and the marker search applies it
	// without a second POST.
	ytCommentsDispatchTimeout = 3 * time.Second
	ytCommentsSettleTimeout   = 1 * time.Second
	ytCommentsPassLimit       = 50
)

// ytCommentsModeFromEnv reads HANDOFFKEEP_YT_COMMENTS_MODE: unset or "off"
// (the default) or "canary". Any other value is an error, so a typo fails
// closed instead of guessing a mode.
func ytCommentsModeFromEnv() (store.YTCommentsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_COMMENTS_MODE")); raw {
	case "", string(store.YTCommentsOff):
		return store.YTCommentsOff, nil
	case string(store.YTCommentsCanary):
		return store.YTCommentsCanary, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_MODE=%q: want off or canary", raw)
	}
}

// ytCommentsWorkerFromEnv sets the store's gate and returns the dispatcher
// for serve's worker list, or nil when the mode is off (which needs no
// YouTrack configuration). canary without YOUTRACK_URL and
// YOUTRACK_BOT_API_KEY is a startup error, never a writer that silently does
// nothing.
func ytCommentsWorkerFromEnv(st *store.Store) (backgroundWorker, error) {
	mode, err := ytCommentsModeFromEnv()
	if err != nil {
		return nil, err
	}
	if mode == store.YTCommentsOff {
		if st != nil {
			st.SetYTCommentsMode(store.YTCommentsOff)
		}
		return nil, nil
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return nil, errors.New("HANDOFFKEEP_YT_COMMENTS_MODE=canary needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY")
	}
	if st == nil {
		return nil, errors.New("HANDOFFKEEP_YT_COMMENTS_MODE=canary needs the store")
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return nil, err
	}
	remote := youtrack.NewWriteRemote(client)
	w, err := newYTCommentsWorker(st, remote, remote.Tenant(), nil)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newYTCommentsWorker turns the gate on and builds the dispatcher for one
// tenant over remote. now is the protocol clock (nil: the wall clock).
func newYTCommentsWorker(st *store.Store, remote ytwrite.Remote, tenant string, now func() time.Time) (ytCommentsWorker, error) {
	p, err := ytwrite.New(st.YTCommentConfig(tenant, ytwrite.Config{
		Remote:          remote,
		Now:             now,
		DispatchTimeout: ytCommentsDispatchTimeout,
		SettleTimeout:   ytCommentsSettleTimeout,
	}))
	if err != nil {
		return ytCommentsWorker{}, err
	}
	st.SetYTCommentsMode(store.YTCommentsCanary)
	log.Printf("ytwrite.comments_enabled mode=canary tenant=%q", tenant)
	return ytCommentsWorker{protocol: p}, nil
}

// ytCommentsWorker runs one sequential Drive pass every interval. Two serve
// processes (blue-green) may both run it: the ledger's compare-and-set claim
// is the boundary, so an op is sent at most once. Its ledger is scoped to the
// tenant it was configured for (store.YTCommentLedger): it neither sends nor
// searches for an op bound to another tenant.
type ytCommentsWorker struct {
	protocol *ytwrite.Protocol
	interval time.Duration
}

func (w ytCommentsWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = ytCommentsInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		st, err := w.protocol.Drive(ctx, ytCommentsPassLimit)
		if err != nil && ctx.Err() == nil {
			log.Printf("ytwrite.drive_failed err=%q", err.Error())
		}
		if st != (ytwrite.DriveStats{}) {
			log.Printf("ytwrite.drive recovered=%d visited=%d reconciled=%d applied=%d", st.Recovered, st.Visited, st.Reconciled, st.Applied)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ytOpsAlertWorker scans the durable ledger independently of dispatch mode,
// so pending ops still alert if comment sending is turned off.
type ytOpsAlertWorker struct {
	store    *store.Store
	interval time.Duration
}

func (w ytOpsAlertWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = ytCommentsInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := w.store.EnqueueYTWriteOpAlerts(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
			log.Printf("ytwrite.alerts_failed err=%q", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

const youtrackOpsUsage = "usage: youtrack ops [--state pending|all|prepared|dispatched|applied|rejected|remote_unknown] [--task N] [--after-id N] [--limit N] | youtrack ops resolve <op id> --applied <youtrack id> | --not-applied --by <actor> --note <text>"

// youtrackOpsCmd is `youtrack ops` (also `yt ops`): the write-protocol ops
// through GET /v1/yt/ops. remote_unknown shows status pending, never done.
func youtrackOpsCmd(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "resolve" {
		return youtrackOpsResolveCmd(args[1:], out)
	}
	fs := flag.NewFlagSet("youtrack ops", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	state := fs.String("state", "pending", "pending (default), all, or one lifecycle state")
	task := fs.Int64("task", 0, "only ops of this task")
	after := fs.Int64("after-id", 0, "ops with id greater than this")
	limit := fs.Int("limit", 100, "maximum results")
	ordered, err := splitArgs(args, "state", "task", "after-id", "limit", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackOpsUsage)
	}
	if s := *state; s != "pending" && s != "all" && !ytwrite.State(s).Valid() {
		return fmt.Errorf("unknown --state %q; %s", s, youtrackOpsUsage)
	}
	if *task < 0 || *after < 0 || *limit < 1 || *limit > store.YTOpListMax {
		return errors.New(youtrackOpsUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ops, next, err := c.ListYTOps(ctx, *state, *task, *after, *limit)
	if err != nil {
		return err
	}
	out2 := map[string]any{"ops": ops, "next_after_id": nil}
	if next > 0 {
		out2["next_after_id"] = next
	}
	return printJSON(out, out2)
}

func youtrackOpsResolveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack ops resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	appliedID := fs.String("applied", "", "YouTrack comment id when the op was applied")
	notApplied := fs.Bool("not-applied", false, "record that the comment was not applied")
	by := fs.String("by", "", "operator who checked YouTrack")
	note := fs.String("note", "", "operator's resolution note")
	c := remoteClient(fs)
	ordered, err := splitArgs(args, "applied", "by", "note", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || (*appliedID != "") == *notApplied {
		return errors.New(youtrackOpsUsage)
	}
	if *notApplied && (strings.TrimSpace(*by) == "" || strings.TrimSpace(*note) == "") {
		return errors.New(youtrackOpsUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("op id must be positive")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	actor := *by
	if actor == "" {
		actor = "operator:cli"
	}
	result, err := c.ResolveYTOp(ctx, id, ytwrite.Resolution{Applied: !*notApplied, YTID: *appliedID, By: actor, Note: *note})
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"op_id": result.OpID, "status": result.Status, "state": result.State, "reason": result.Reason, "yt_id": result.YTID})
}

const youtrackCanaryUsage = "usage: youtrack canary add <task> [--by NAME] | remove <task> | list"

// youtrackCanaryCmd keeps the comment canary set (slice b1a) directly in PG,
// like `youtrack projects`. add verifies the task's imported issue in
// YouTrack first and stores the binding; it writes no op row.
func youtrackCanaryCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackCanaryUsage)
	}
	fs := flag.NewFlagSet("youtrack canary", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	by := fs.String("by", "operator:cli", "who enrolled the task")
	ordered, err := splitArgs(args[1:], "by")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	var taskID int64
	switch args[0] {
	case "add", "remove":
		if fs.NArg() != 1 {
			return errors.New(youtrackCanaryUsage)
		}
		if taskID, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || taskID < 1 {
			return errors.New("task id must be positive")
		}
	case "list":
		if fs.NArg() != 0 {
			return errors.New(youtrackCanaryUsage)
		}
	default:
		return errors.New(youtrackCanaryUsage)
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "list":
		set, err := st.ListYTCommentCanary(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"canary": set})
	case "remove":
		removed, err := st.RemoveYTCommentCanary(ctx, taskID)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"task_id": taskID, "removed": removed})
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required to verify the issue")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	remote := youtrack.NewWriteRemote(client)
	candidate, err := st.YTCanaryCandidate(ctx, taskID)
	if err != nil {
		return err
	}
	binding, err := youtrack.VerifyCanary(ctx, remote, remote.Tenant(), candidate)
	if err != nil {
		return err
	}
	binding.AddedBy = *by
	got, err := st.AddYTCommentCanary(ctx, binding, candidate)
	if err != nil {
		return err
	}
	return printJSON(out, got)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Slice b1a (hk 3739) and mode on (hk 3810): a new hk comment on a canary
// task, or in mode on on a task bound to its issue, also gets a
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
// (the default), "canary" or "on", trimmed and exactly in lower case. Any
// other value is an error, so a typo fails closed instead of guessing a mode.
func ytCommentsModeFromEnv() (store.YTCommentsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_COMMENTS_MODE")); raw {
	case "", string(store.YTCommentsOff):
		return store.YTCommentsOff, nil
	case string(store.YTCommentsCanary):
		return store.YTCommentsCanary, nil
	case string(store.YTCommentsOn):
		return store.YTCommentsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_MODE=%q: want off, canary or on", raw)
	}
}

// ytCommentsWorkerFromEnv sets the store's gate and returns the dispatcher
// for serve's worker list, or nil when the mode is off (which needs no
// YouTrack configuration). canary or on without YOUTRACK_URL and
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
		return nil, fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_MODE=%s needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY", mode)
	}
	if st == nil {
		return nil, fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_MODE=%s needs the store", mode)
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return nil, err
	}
	remote := youtrack.NewWriteRemote(client)
	w, err := newYTCommentsWorker(st, remote, remote.Tenant(), mode, nil)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newYTCommentsWorker sets the gate to mode (canary or on) and builds the
// dispatcher for one tenant over remote. now is the protocol clock (nil: the
// wall clock).
func newYTCommentsWorker(st *store.Store, remote ytwrite.Remote, tenant string, mode store.YTCommentsMode, now func() time.Time) (ytCommentsWorker, error) {
	if mode != store.YTCommentsCanary && mode != store.YTCommentsOn {
		return ytCommentsWorker{}, fmt.Errorf("the comment worker runs in mode canary or on, not %q", mode)
	}
	p, err := ytwrite.New(st.YTCommentConfig(tenant, ytwrite.Config{
		Remote:          remote,
		Now:             now,
		DispatchTimeout: ytCommentsDispatchTimeout,
		SettleTimeout:   ytCommentsSettleTimeout,
	}))
	if err != nil {
		return ytCommentsWorker{}, err
	}
	st.SetYTCommentsMode(mode)
	log.Printf("ytwrite.comments_enabled mode=%s tenant=%q", mode, tenant)
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

// Comment mode on is turned on for tasks with a verified binding
// (yt_task_bindings). `youtrack comments-preflight` reports which tasks would
// be covered and writes nothing; `youtrack comments-backfill` makes the same
// classification and binds the tasks that verify. Both open the database
// directly, like `youtrack canary`, and verify against the tenant the comment
// worker uses (the host of YOUTRACK_URL). docs/youtrack-write-protocol.md.

// The classes of a candidate task.
const (
	// ytClassBound: bound on this tenant and every local source agrees.
	ytClassBound = "bound"
	// ytClassBoundAmbiguous: bound, but a local source disagrees with the
	// binding (or it is on another tenant): mode on skips its comments.
	ytClassBoundAmbiguous = "bound_ambiguous"
	// ytClassVerifies: not bound, and its issue verifies in YouTrack.
	ytClassVerifies = "verifies"
	// ytClassNoIssue: no project, an unmapped project, or no such issue.
	ytClassNoIssue = "no_issue"
	// ytClassAmbiguous: a refusal caused by a disagreement, or a binding
	// the publication refused.
	ytClassAmbiguous = "ambiguous"
	// ytClassError: a read failed; run again.
	ytClassError = "error"
)

const (
	ytCommentsDefaultDays = 14
	// ytCommentsSample bounds the task ids listed per class.
	ytCommentsSample = 10
	// ytCommentsGetTimeout bounds one YouTrack read; ytCommentsTaskTimeout
	// one task's classification (its reads and that GET) and its binding.
	// There is no budget for the whole run, so a long run never stops
	// half-way.
	ytCommentsGetTimeout  = 15 * time.Second
	ytCommentsTaskTimeout = 60 * time.Second
)

// ytCommentsClass is one class of one project: how many tasks, and the first
// ytCommentsSample ids in task id order.
type ytCommentsClass struct {
	Count   int     `json:"count"`
	TaskIDs []int64 `json:"task_ids"`
}

func (c *ytCommentsClass) add(taskID int64) {
	c.Count++
	if len(c.TaskIDs) < ytCommentsSample {
		c.TaskIDs = append(c.TaskIDs, taskID)
	}
}

type ytCommentsProject struct {
	Project        string          `json:"project"`
	Bound          ytCommentsClass `json:"bound"`
	BoundAmbiguous ytCommentsClass `json:"bound_ambiguous"`
	Verifies       ytCommentsClass `json:"verifies"`
	NoIssue        ytCommentsClass `json:"no_issue"`
	Ambiguous      ytCommentsClass `json:"ambiguous"`
	Error          ytCommentsClass `json:"error"`
}

func (p *ytCommentsProject) class(name string) *ytCommentsClass {
	switch name {
	case ytClassBound:
		return &p.Bound
	case ytClassBoundAmbiguous:
		return &p.BoundAmbiguous
	case ytClassVerifies:
		return &p.Verifies
	case ytClassNoIssue:
		return &p.NoIssue
	case ytClassAmbiguous:
		return &p.Ambiguous
	default:
		return &p.Error
	}
}

// ytCommentsReport is the output of both commands. BoundNow is set by the
// backfill only.
type ytCommentsReport struct {
	Tenant             string               `json:"tenant"`
	Days               int                  `json:"days"`
	Project            string               `json:"project,omitempty"`
	CommentsTotal      int                  `json:"comments_total"`
	MaxCommentsPerHour int                  `json:"max_comments_per_hour"`
	Projects           []*ytCommentsProject `json:"projects"`
	Uncovered          struct {
		Count int     `json:"count"`
		OpIDs []int64 `json:"op_ids"`
	} `json:"unresolved_ops_uncovered"`
	BoundNow *int `json:"bound_now,omitempty"`
	errors   int
}

// ytClassified is one task's class, with the binding and the candidate it
// was verified against when the class is verifies.
type ytClassified struct {
	class    string
	detail   string
	binding  store.YTTaskBinding
	verified store.YTCanaryCandidate
}

// classifyYTCommentTask classifies one task. A bound task is checked against
// the local sources only (no YouTrack call); an unbound one is verified in
// YouTrack (youtrack.VerifyBinding) and its would-be binding checked against
// the same local sources.
func classifyYTCommentTask(ctx context.Context, st *store.Store, issues youtrack.IssueReader, tenant string, taskID int64) ytClassified {
	failed := func(err error) ytClassified { return ytClassified{class: ytClassError, detail: err.Error()} }
	b, found, err := st.YTTaskBinding(ctx, taskID)
	if err != nil {
		return failed(err)
	}
	if found {
		if b.Tenant != tenant {
			return ytClassified{class: ytClassBoundAmbiguous, detail: "bound on tenant " + b.Tenant}
		}
		source, err := st.YTBindingDisagreement(ctx, b)
		if err != nil {
			return failed(err)
		}
		if source != "" {
			return ytClassified{class: ytClassBoundAmbiguous, detail: source + " disagrees with the binding"}
		}
		return ytClassified{class: ytClassBound}
	}
	verified, err := st.YTCanaryCandidate(ctx, taskID)
	if err != nil {
		return failed(err)
	}
	gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
	b, err = youtrack.VerifyBinding(gctx, issues, tenant, verified)
	cancel()
	switch {
	case errors.Is(err, youtrack.ErrNoIssue):
		return ytClassified{class: ytClassNoIssue, detail: err.Error()}
	case errors.Is(err, youtrack.ErrCanaryRefused):
		return ytClassified{class: ytClassAmbiguous, detail: err.Error()}
	case err != nil:
		return failed(err)
	}
	source, err := st.YTBindingDisagreement(ctx, b)
	if err != nil {
		return failed(err)
	}
	if source != "" {
		return ytClassified{class: ytClassAmbiguous, detail: source + " disagrees with issue " + b.IssueID}
	}
	return ytClassified{class: ytClassVerifies, binding: b, verified: verified}
}

// ytCommentsRun classifies every candidate and, when bindBy is not "" (the
// backfill), publishes each verified binding; a refused publication counts
// as ambiguous and the run goes on. It never stops on one task.
func ytCommentsRun(ctx context.Context, st *store.Store, issues youtrack.IssueReader, tenant, project string, days int, bindBy string) (ytCommentsReport, error) {
	r := ytCommentsReport{Tenant: tenant, Days: days, Project: project, Projects: []*ytCommentsProject{}}
	r.Uncovered.OpIDs = []int64{}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var err error
	if r.CommentsTotal, r.MaxCommentsPerHour, err = st.YTCommentVolume(ctx, since, project); err != nil {
		return r, err
	}
	candidates, err := st.YTCommentCandidates(ctx, since, project)
	if err != nil {
		return r, err
	}
	byProject := map[string]*ytCommentsProject{}
	candidate := map[int64]bool{}
	boundNow := 0
	for _, c := range candidates {
		candidate[c.TaskID] = true
		tctx, cancel := context.WithTimeout(ctx, ytCommentsTaskTimeout)
		got := classifyYTCommentTask(tctx, st, issues, tenant, c.TaskID)
		if bindBy != "" && got.class == ytClassVerifies {
			b := got.binding
			b.BoundBy = bindBy
			_, err := st.AddYTTaskBinding(tctx, b, got.verified)
			switch {
			case err == nil:
				boundNow++
			case errors.Is(err, store.ErrYTBindingConflict):
				got = ytClassified{class: ytClassAmbiguous, detail: err.Error()}
			default:
				got = ytClassified{class: ytClassError, detail: err.Error()}
			}
		}
		cancel()
		switch got.class {
		case ytClassError:
			r.errors++
			log.Printf("youtrack.comments_classify_failed task_id=%d err=%q", c.TaskID, got.detail)
		case ytClassBoundAmbiguous, ytClassNoIssue, ytClassAmbiguous:
			log.Printf("youtrack.comments_classified task_id=%d class=%s detail=%q", c.TaskID, got.class, got.detail)
		}
		p := byProject[c.Project]
		if p == nil {
			p = &ytCommentsProject{Project: c.Project}
			for _, name := range []string{ytClassBound, ytClassBoundAmbiguous, ytClassVerifies, ytClassNoIssue, ytClassAmbiguous, ytClassError} {
				p.class(name).TaskIDs = []int64{}
			}
			byProject[c.Project] = p
			r.Projects = append(r.Projects, p)
		}
		p.class(got.class).add(c.TaskID)
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Project < r.Projects[j].Project })
	if bindBy != "" {
		r.BoundNow = &boundNow
	}
	// What mode on would hold: unresolved comment ops of candidate tasks
	// with no binding on this tenant that matches the op and agrees with
	// every local source. Read after the bindings, so a backfill reports
	// what is left.
	ops, err := st.YTUnresolvedCommentOps(ctx)
	if err != nil {
		return r, err
	}
	covered := map[int64]store.YTTaskBinding{}
	for _, op := range ops {
		if !candidate[op.TaskID] {
			continue
		}
		b, seen := covered[op.TaskID]
		if !seen {
			got, found, err := st.YTTaskBinding(ctx, op.TaskID)
			if err != nil {
				return r, err
			}
			if found && got.Tenant == tenant {
				source, err := st.YTBindingDisagreement(ctx, got)
				if err != nil {
					return r, err
				}
				if source == "" {
					b = got
				}
			}
			covered[op.TaskID] = b
		}
		if op.Tenant != tenant || b.Tenant != op.Tenant || b.IssueID != op.IssueID {
			r.Uncovered.Count++
			if len(r.Uncovered.OpIDs) < ytCommentsSample {
				r.Uncovered.OpIDs = append(r.Uncovered.OpIDs, op.OpID)
			}
		}
	}
	return r, nil
}

const (
	youtrackCommentsPreflightUsage = "usage: youtrack comments-preflight [--project P] [--days N]"
	youtrackCommentsBackfillUsage  = "usage: youtrack comments-backfill [--project P] [--days N] [--by NAME]"
)

// youtrackCommentsCmd is `youtrack comments-preflight` (backfill false):
// read-only, it reads YouTrack issues but writes nothing; and `youtrack
// comments-backfill` (backfill true), which also publishes each verified
// binding. The report goes to stdout; a task whose reads failed (class error)
// makes the command exit 1 after the report.
func youtrackCommentsCmd(args []string, out io.Writer, backfill bool) error {
	name, usage := "comments-preflight", youtrackCommentsPreflightUsage
	if backfill {
		name, usage = "comments-backfill", youtrackCommentsBackfillUsage
	}
	fs := flag.NewFlagSet("youtrack "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	days := fs.Int("days", ytCommentsDefaultDays, "tasks with a comment in the last N days")
	valueFlags := []string{"project", "days"}
	var by *string
	if backfill {
		by = fs.String("by", "operator:cli", "who bound the tasks")
		valueFlags = append(valueFlags, "by")
	}
	ordered, err := splitArgs(args, valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, usage)
	}
	if fs.NArg() != 0 || *days < 1 {
		return errors.New(usage)
	}
	bindBy := ""
	if backfill {
		if bindBy = strings.TrimSpace(*by); bindBy == "" {
			return errors.New(usage)
		}
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
	remote := youtrack.NewWriteRemote(client)
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	report, err := ytCommentsRun(context.Background(), st, remote, remote.Tenant(), *project, *days, bindBy)
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.errors > 0 {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d tasks could not be classified (class error); run again", report.errors)}
	}
	return nil
}

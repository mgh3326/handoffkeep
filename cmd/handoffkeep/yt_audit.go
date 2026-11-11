package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `handoffkeep youtrack audit` (hk 3899, docs/youtrack-write-protocol.md
// §Comment audit) compares every bound task's hk comments with the comments
// on its YouTrack issue and counts the tasks with no binding. It is the gate
// for the comment cutover: missing_in_yt, duplicate and unbound all 0 (or
// explained). Read-only by default; --enqueue prepares the op of each
// missing comment through the enqueue path and changes nothing else. It
// never binds, deletes or edits anything in YouTrack.

// youtrackExitAuditGap is the audit's exit when it ran in full and found a
// missing_in_yt or duplicate comment or an unbound task.
const youtrackExitAuditGap = 4

const (
	// ytAuditSample bounds the items listed per class.
	ytAuditSample = 10
	// ytAuditPageSize is the comment listing's page.
	ytAuditPageSize = 100
	// ytAuditCallTimeout bounds one remote read: an issue GET, or one
	// issue's full listing (two full reads per attempt).
	ytAuditCallTimeout = 60 * time.Second
	// ytAuditRateLimitWait is the wait after a 429 without Retry-After, the
	// protocol's own (ytwrite.Config.RateLimitWait).
	ytAuditRateLimitWait = 5 * time.Second
	// ytAuditMaxWait is the longest Retry-After the audit waits out; a
	// longer one stops the run (exit 1), and the next run starts again.
	ytAuditMaxWait = 10 * time.Minute
	// ytAuditRateLimitAttempts bounds the reads of one call that a 429 may
	// repeat.
	ytAuditRateLimitAttempts = 5
)

// ytAuditTaskPage is how many tasks the run lists at a time; it pages
// through every task (a variable so a test can page a few).
var ytAuditTaskPage = store.YTAuditTaskPageMax

type ytAuditItem struct {
	TaskID    int64    `json:"task_id"`
	CommentID int64    `json:"comment_id,omitempty"`
	OpID      int64    `json:"op_id,omitempty"`
	YTIDs     []string `json:"yt_ids,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

// ytAuditClass is a count and its first ytAuditSample items, in the order
// the run met them (task id, then comment id or YouTrack id).
type ytAuditClass struct {
	Count int           `json:"count"`
	Items []ytAuditItem `json:"items"`
}

func (c *ytAuditClass) add(it ytAuditItem) {
	c.Count++
	if len(c.Items) < ytAuditSample {
		c.Items = append(c.Items, it)
	}
}

// ytAuditUnbound is the tasks with no binding the audit can use, by reason.
type ytAuditUnbound struct {
	Count int `json:"count"`
	// NoProject: the task has no hk project, so there is no issue to bind.
	NoProject ytAuditClass `json:"no_project"`
	// NoIssue: an unmapped project, a project with no YouTrack id, or no
	// such issue.
	NoIssue ytAuditClass `json:"no_issue"`
	// Ambiguous: the issue does not verify, or a local source disagrees.
	Ambiguous ytAuditClass `json:"ambiguous"`
	// Verifies: the issue verifies; comments-backfill has not bound it.
	Verifies ytAuditClass `json:"verifies"`
	// OtherTenant: bound on another YouTrack than this run's.
	OtherTenant ytAuditClass `json:"other_tenant"`
}

const (
	ytUnboundNoProject   = "no_project"
	ytUnboundNoIssue     = "no_issue"
	ytUnboundAmbiguous   = "ambiguous"
	ytUnboundVerifies    = "verifies"
	ytUnboundOtherTenant = "other_tenant"
)

func (u *ytAuditUnbound) add(reason string, it ytAuditItem) {
	u.Count++
	switch reason {
	case ytUnboundNoProject:
		u.NoProject.add(it)
	case ytUnboundNoIssue:
		u.NoIssue.add(it)
	case ytUnboundVerifies:
		u.Verifies.add(it)
	case ytUnboundOtherTenant:
		u.OtherTenant.add(it)
	default:
		u.Ambiguous.add(it)
	}
}

// ytAuditCounts is one project's (or the total's) classes. Comments are
// counted once each: present, missing_in_yt or duplicate for an hk comment,
// yt_only_bot or yt_only_human for a YouTrack comment that copies none.
type ytAuditCounts struct {
	// TasksAudited is the bound tasks whose comments were compared.
	TasksAudited int          `json:"tasks_audited"`
	Present      ytAuditClass `json:"present"`
	MissingInYT  ytAuditClass `json:"missing_in_yt"`
	Duplicate    ytAuditClass `json:"duplicate"`
	// AdoptedUnmatched is hk comments the adoption says the Python mirror
	// posted, with no copy hk can identify: never enqueued, for the operator.
	AdoptedUnmatched ytAuditClass   `json:"adopted_unmatched"`
	YTOnlyBot        ytAuditClass   `json:"yt_only_bot"`
	YTOnlyHuman      ytAuditClass   `json:"yt_only_human"`
	Unbound          ytAuditUnbound `json:"unbound"`
	// Error is the tasks whose reads failed; run again.
	Error ytAuditClass `json:"error"`
}

type ytAuditProject struct {
	Project string `json:"project"`
	ytAuditCounts
}

// ytAuditEnqueue is what --enqueue did with the missing_in_yt comments.
type ytAuditEnqueue struct {
	// Prepared: a new op; Linked: the comment already had one (nothing
	// written); Skipped: the enqueue's rule wrote nothing (the binding
	// disagrees with a source); Failed: a database error.
	Prepared int     `json:"prepared"`
	Linked   int     `json:"linked"`
	Skipped  int     `json:"skipped"`
	Failed   int     `json:"failed"`
	OpIDs    []int64 `json:"op_ids"`
}

type ytAuditReport struct {
	Tenant  string `json:"tenant"`
	Project string `json:"project,omitempty"`
	Task    int64  `json:"task,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	// Tasks is how many tasks the run read; Stopped, when set, says why it
	// ended before the last one.
	Tasks    int               `json:"tasks"`
	Stopped  string            `json:"stopped,omitempty"`
	Total    ytAuditCounts     `json:"total"`
	Projects []*ytAuditProject `json:"projects"`
	Enqueue  *ytAuditEnqueue   `json:"enqueue,omitempty"`
}

// gap reports whether the run found what the cutover gate refuses.
func (r *ytAuditReport) gap() bool {
	return r.Total.MissingInYT.Count > 0 || r.Total.Duplicate.Count > 0 || r.Total.AdoptedUnmatched.Count > 0 || r.Total.Unbound.Count > 0
}

// ytAuditRemote is the YouTrack the audit reads: one issue, and one issue's
// comments (youtrack.WriteRemote).
type ytAuditRemote interface {
	youtrack.IssueReader
	ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error)
}

// ytAuditor runs one audit. Tasks are read one at a time, and each remote
// read waits out a 429 as the protocol does: no request before the server's
// Retry-After (ytAuditRateLimitWait without one).
type ytAuditor struct {
	st      *store.Store
	remote  ytAuditRemote
	tenant  string
	botID   string
	enqueue bool
	// sleep waits d or until ctx ends (nil: the wall clock).
	sleep func(ctx context.Context, d time.Duration) error
}

// errYTAuditStop ends the run: a read the server will not take for longer
// than the audit waits.
type errYTAuditStop struct{ reason string }

func (e errYTAuditStop) Error() string { return e.reason }

func (a *ytAuditor) wait(ctx context.Context, d time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// call runs one remote read, repeating it after a 429 once the server's wait
// has passed.
func (a *ytAuditor) call(ctx context.Context, read func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, ytAuditCallTimeout)
		err := read(cctx)
		cancel()
		var re *ytwrite.RemoteError
		if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests {
			return err
		}
		wait := re.RetryAfter
		if wait <= 0 {
			wait = ytAuditRateLimitWait
		}
		if wait > ytAuditMaxWait || attempt >= ytAuditRateLimitAttempts {
			return errYTAuditStop{reason: fmt.Sprintf("rate_limited: YouTrack asked to wait %s after %d attempts", wait, attempt)}
		}
		if err := a.wait(ctx, wait); err != nil {
			return err
		}
	}
}

// unbound classifies a task with no binding. It never binds: binding stays
// with comments-backfill.
func (a *ytAuditor) unbound(ctx context.Context, taskID int64) (reason, detail string, err error) {
	c, err := a.st.YTCanaryCandidate(ctx, taskID)
	if err != nil {
		return "", "", err
	}
	if c.Project == "" {
		return ytUnboundNoProject, "", nil
	}
	var b store.YTTaskBinding
	err = a.call(ctx, func(ctx context.Context) error {
		var verr error
		b, verr = youtrack.VerifyBinding(ctx, a.remote, a.tenant, c)
		return verr
	})
	switch {
	case errors.Is(err, youtrack.ErrNoIssue):
		return ytUnboundNoIssue, err.Error(), nil
	case errors.Is(err, youtrack.ErrCanaryRefused):
		return ytUnboundAmbiguous, err.Error(), nil
	case err != nil:
		return "", "", err
	}
	source, err := a.st.YTBindingDisagreement(ctx, b)
	if err != nil {
		return "", "", err
	}
	if source != "" {
		return ytUnboundAmbiguous, source + " disagrees with issue " + b.IssueID, nil
	}
	return ytUnboundVerifies, "", nil
}

// auditTask audits one task into counts (the project's and the total).
func (a *ytAuditor) auditTask(ctx context.Context, taskID int64, counts []*ytAuditCounts, enq *ytAuditEnqueue) error {
	each := func(f func(c *ytAuditCounts)) {
		for _, c := range counts {
			f(c)
		}
	}
	b, found, err := a.st.YTTaskBinding(ctx, taskID)
	if err != nil {
		return err
	}
	if found && b.Tenant != a.tenant {
		each(func(c *ytAuditCounts) {
			c.Unbound.add(ytUnboundOtherTenant, ytAuditItem{TaskID: taskID, Detail: "bound on " + b.Tenant})
		})
		return nil
	}
	if !found {
		reason, detail, err := a.unbound(ctx, taskID)
		if err != nil {
			return err
		}
		each(func(c *ytAuditCounts) { c.Unbound.add(reason, ytAuditItem{TaskID: taskID, Detail: detail}) })
		return nil
	}
	hk, err := a.st.YTAuditTaskComments(ctx, taskID, b.IssueID)
	if err != nil {
		return err
	}
	var listed []ytwrite.RemoteComment
	if err := a.call(ctx, func(ctx context.Context) error {
		var lerr error
		listed, lerr = a.remote.ListComments(ctx, b.IssueID, ytAuditPageSize)
		return lerr
	}); err != nil {
		return err
	}
	// A YouTrack comment the activity pull copied into hk came from YouTrack:
	// it is not hk's to account for. Only the linked ids are known; an equal
	// text is no evidence (two people may write the same words).
	yt := listed[:0:0]
	for _, c := range listed {
		if !hk.PulledYTIDs[c.ID] {
			yt = append(yt, c)
		}
	}
	// The legacy mirror push's evidence, read by the sync's own rules: a
	// recorded link that is a YouTrack id, and a comment carrying the
	// <!-- hk-comment:N --> marker of one of this task's comments.
	index := map[int64]int{}
	for i, c := range hk.Comments {
		index[c.ID] = i
		if link := hk.Links[c.ID]; youtrack.CommentLinkIsID(link) {
			hk.Comments[i].KnownYTIDs = append(hk.Comments[i].KnownYTIDs, link)
		}
	}
	for _, c := range yt {
		if id, ok := youtrack.LegacyCommentID(c.Text); ok {
			if i, ok := index[id]; ok && !slices.Contains(hk.Comments[i].KnownYTIDs, c.ID) {
				hk.Comments[i].KnownYTIDs = append(hk.Comments[i].KnownYTIDs, c.ID)
			}
		}
	}
	got := ytwrite.AuditComments(hk.Comments, yt, a.botID)
	// A comment the adoption linked was posted by the Python mirror in a form
	// hk cannot recognise; with no copy found it is adopted_unmatched, for
	// the operator, and never enqueued: a second copy may be the result.
	missing := got.Missing[:0:0]
	var adopted []ytwrite.AuditCopies
	for _, x := range got.Missing {
		if youtrack.CommentLinkIsAdopted(hk.Links[x.CommentID]) {
			adopted = append(adopted, x)
		} else {
			missing = append(missing, x)
		}
	}
	item := func(x ytwrite.AuditCopies) ytAuditItem {
		return ytAuditItem{TaskID: taskID, CommentID: x.CommentID, OpID: x.OpID, YTIDs: x.YTIDs}
	}
	each(func(c *ytAuditCounts) {
		c.TasksAudited++
		for _, x := range got.Present {
			c.Present.add(item(x))
		}
		for _, x := range missing {
			c.MissingInYT.add(item(x))
		}
		for _, x := range adopted {
			c.AdoptedUnmatched.add(item(x))
		}
		for _, x := range got.Duplicate {
			c.Duplicate.add(item(x))
		}
		for _, id := range got.YTOnlyBot {
			c.YTOnlyBot.add(ytAuditItem{TaskID: taskID, YTIDs: []string{id}})
		}
		for _, id := range got.YTOnlyHuman {
			c.YTOnlyHuman.add(ytAuditItem{TaskID: taskID, YTIDs: []string{id}})
		}
	})
	if !a.enqueue {
		return nil
	}
	for _, x := range missing {
		status, op, err := a.st.EnqueueYTAuditComment(ctx, x.CommentID)
		switch {
		case err != nil:
			enq.Failed++
			log.Printf("youtrack.audit_enqueue_failed task_id=%d comment_id=%d err=%q", taskID, x.CommentID, err.Error())
		case status == store.YTAuditPrepared:
			enq.Prepared++
			if len(enq.OpIDs) < ytAuditSample {
				enq.OpIDs = append(enq.OpIDs, op.ID)
			}
		case status == store.YTAuditLinked:
			enq.Linked++
		default:
			enq.Skipped++
		}
	}
	return nil
}

// run audits the tasks of project (or task taskID), at most limit (0: all),
// in task id order, listing them a page at a time. One task's failed read is class error and the run goes
// on; a rate limit longer than the audit waits stops it.
func (a *ytAuditor) run(ctx context.Context, project string, taskID int64, limit int) (ytAuditReport, error) {
	r := ytAuditReport{Tenant: a.tenant, Project: project, Task: taskID, Limit: limit, Projects: []*ytAuditProject{}}
	if a.enqueue {
		r.Enqueue = &ytAuditEnqueue{OpIDs: []int64{}}
	}
	byProject := map[string]*ytAuditProject{}
	var after int64
	for {
		page := ytAuditTaskPage
		if limit > 0 {
			page = min(page, limit-r.Tasks)
		}
		if page < 1 || r.Stopped != "" {
			break
		}
		tasks, err := a.st.YTAuditTasks(ctx, project, taskID, after, page)
		if err != nil {
			return r, err
		}
		for _, t := range tasks {
			after = t.TaskID
			p := byProject[t.Project]
			if p == nil {
				p = &ytAuditProject{Project: t.Project}
				byProject[t.Project] = p
				r.Projects = append(r.Projects, p)
			}
			r.Tasks++
			err := a.auditTask(ctx, t.TaskID, []*ytAuditCounts{&p.ytAuditCounts, &r.Total}, r.Enqueue)
			if err == nil {
				continue
			}
			p.Error.add(ytAuditItem{TaskID: t.TaskID, Detail: err.Error()})
			r.Total.Error.add(ytAuditItem{TaskID: t.TaskID, Detail: err.Error()})
			log.Printf("youtrack.audit_task_failed task_id=%d err=%q", t.TaskID, err.Error())
			var stop errYTAuditStop
			if errors.As(err, &stop) || ctx.Err() != nil {
				r.Stopped = err.Error()
				break
			}
		}
		if len(tasks) < page {
			break
		}
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Project < r.Projects[j].Project })
	for _, p := range append([]*ytAuditCounts{&r.Total}, projectCounts(r.Projects)...) {
		p.fillEmpty()
	}
	return r, nil
}

func projectCounts(ps []*ytAuditProject) []*ytAuditCounts {
	out := make([]*ytAuditCounts, 0, len(ps))
	for _, p := range ps {
		out = append(out, &p.ytAuditCounts)
	}
	return out
}

// fillEmpty makes every empty item list [] rather than null.
func (c *ytAuditCounts) fillEmpty() {
	for _, x := range []*ytAuditClass{&c.Present, &c.MissingInYT, &c.Duplicate, &c.AdoptedUnmatched, &c.YTOnlyBot, &c.YTOnlyHuman, &c.Error,
		&c.Unbound.NoProject, &c.Unbound.NoIssue, &c.Unbound.Ambiguous, &c.Unbound.Verifies, &c.Unbound.OtherTenant} {
		if x.Items == nil {
			x.Items = []ytAuditItem{}
		}
	}
}

const youtrackAuditUsage = "usage: youtrack audit [--project P] [--task N] [--enqueue] [--limit N]"

// youtrackAuditCmd is `youtrack audit`. It opens the database directly and
// reads YouTrack with the bot key, like comments-backfill, against the tenant
// the comment worker uses (the host of YOUTRACK_URL). Without --enqueue it
// writes nothing (store.OpenReadOnly). The report goes to stdout; then a
// task whose reads failed, a stopped run or a failed enqueue exits 1, and a
// missing_in_yt or duplicate comment or an unbound task exits
// youtrackExitAuditGap.
func youtrackAuditCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	task := fs.Int64("task", 0, "only this task")
	enqueue := fs.Bool("enqueue", false, "prepare the op of each missing_in_yt comment")
	limit := fs.Int("limit", 0, "audit at most N tasks (0: all)")
	ordered, err := splitArgs(args, "project", "task", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackAuditUsage)
	}
	if fs.NArg() != 0 || *task < 0 || *limit < 0 {
		return errors.New(youtrackAuditUsage)
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
	// Read-only unless --enqueue: no migration, every transaction READ ONLY.
	// --enqueue writes ops and migrates as the server and the backfill do.
	openStore := store.OpenReadOnly
	if *enqueue {
		openStore = store.Open
	}
	st, err := openStore(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if !*enqueue {
		applied, err := st.MigrationApplied(openCtx, store.YTTaskBindingsMigration)
		if err != nil {
			return err
		}
		if !applied {
			return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the audit never migrates", store.YTTaskBindingsMigration)}
		}
	}
	me, err := client.Me(openCtx)
	if err != nil {
		return fmt.Errorf("read the bot's YouTrack user: %w", err)
	}
	a := &ytAuditor{st: st, remote: remote, tenant: remote.Tenant(), botID: me.ID, enqueue: *enqueue}
	report, err := a.run(context.Background(), *project, *task, *limit)
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	failed := report.Total.Error.Count
	if report.Enqueue != nil {
		failed += report.Enqueue.Failed
	}
	switch {
	case failed > 0 || report.Stopped != "":
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("the audit is incomplete (%d failures, stopped %q); run again", failed, report.Stopped)}
	case report.gap():
		return exitCodeError{code: youtrackExitAuditGap, err: fmt.Errorf("audit gap: %d missing_in_yt, %d duplicate, %d adopted_unmatched, %d unbound",
			report.Total.MissingInYT.Count, report.Total.Duplicate.Count, report.Total.AdoptedUnmatched.Count, report.Total.Unbound.Count)}
	}
	return nil
}

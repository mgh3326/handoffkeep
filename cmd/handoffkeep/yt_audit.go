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
	TaskID    int64 `json:"task_id"`
	CommentID int64 `json:"comment_id,omitempty"`
	// CommentIDs are the hk comments an ambiguous_copy's evidence names.
	CommentIDs []int64  `json:"comment_ids,omitempty"`
	OpID       int64    `json:"op_id,omitempty"`
	YTIDs      []string `json:"yt_ids,omitempty"`
	Detail     string   `json:"detail,omitempty"`
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
// doc_comment for a document copy the import posted, yt_only_bot or
// yt_only_human for a YouTrack comment that copies none.
type ytAuditCounts struct {
	// TasksAudited is the bound tasks whose comments were compared.
	TasksAudited int          `json:"tasks_audited"`
	Present      ytAuditClass `json:"present"`
	MissingInYT  ytAuditClass `json:"missing_in_yt"`
	Duplicate    ytAuditClass `json:"duplicate"`
	// AdoptedUnmatched is hk comments the adoption says the Python mirror
	// posted, with no copy hk can identify: never enqueued, for the operator.
	AdoptedUnmatched ytAuditClass `json:"adopted_unmatched"`
	// AmbiguousCopy is YouTrack comments that are one hk comment's exact
	// body and end in another's legacy trailer: assigned to neither, and
	// neither comment is enqueued (when it has no other copy it is listed
	// here only, not as missing_in_yt).
	AmbiguousCopy ytAuditClass `json:"ambiguous_copy"`
	// DocComment is a document copy the import posted on the issue
	// (ytwrite.ImportedDocCommentKey): not an hk comment, never enqueued.
	DocComment  ytAuditClass   `json:"doc_comment"`
	YTOnlyBot   ytAuditClass   `json:"yt_only_bot"`
	YTOnlyHuman ytAuditClass   `json:"yt_only_human"`
	Unbound     ytAuditUnbound `json:"unbound"`
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

// Repair directions of an audit (hk 4122): which side a gap is repaired
// from, by the comments authority.
const (
	// ytAuditRepairToYT: hk is canonical; --enqueue copies a missing_in_yt
	// comment to YouTrack.
	ytAuditRepairToYT = "pg_to_youtrack"
	// ytAuditRepairToPG: YouTrack is canonical; a gap is repaired from
	// YouTrack into PG, and --enqueue prepares nothing.
	ytAuditRepairToPG = "youtrack_to_pg"
)

type ytAuditReport struct {
	Tenant string `json:"tenant"`
	// RepairDirection is ytAuditRepairToYT or ytAuditRepairToPG.
	RepairDirection string `json:"repair_direction"`
	Project         string `json:"project,omitempty"`
	Task            int64  `json:"task,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	// Tasks is how many tasks the run read; Stopped, when set, says why it
	// ended before the last one.
	Tasks    int               `json:"tasks"`
	Stopped  string            `json:"stopped,omitempty"`
	Total    ytAuditCounts     `json:"total"`
	Projects []*ytAuditProject `json:"projects"`
	Enqueue  *ytAuditEnqueue   `json:"enqueue,omitempty"`
	// EnqueueSkipped says why --enqueue prepared nothing (the comments
	// authority is youtrack).
	EnqueueSkipped string `json:"enqueue_skipped,omitempty"`
	// Documents counts the documents with no article binding on this
	// tenant (hk 4053); absent with --task, or without the articles
	// migration.
	Documents *ytAuditDocuments `json:"documents,omitempty"`
}

// ytAuditDocuments is the documents the import made an article for
// (store.YTArticleExcluded false) that have no yt_article_bindings row on
// the audit's tenant, in all and per project (an hk project, /shared or
// /unassigned). It is a count for the document cutover, not part of the
// comment gate: it does not change the exit.
type ytAuditDocuments struct {
	Documents int                  `json:"documents"`
	Unbound   ytArticlesClass      `json:"unbound"`
	Projects  []*ytAuditDocProject `json:"projects"`
}

type ytAuditDocProject struct {
	Project string          `json:"project"`
	Unbound ytArticlesClass `json:"unbound"`
}

// documents counts the unbound documents of bucket ("" for all).
func (a *ytAuditor) documents(ctx context.Context, bucket string) (*ytAuditDocuments, error) {
	out := &ytAuditDocuments{Unbound: ytArticlesClass{DocumentIDs: []int64{}}, Projects: []*ytAuditDocProject{}}
	byProject := map[string]*ytAuditDocProject{}
	var after int64
	for {
		docs, err := a.st.YTArticleDocs(ctx, bucket, after, ytArticleDocPage)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			after = d.DocumentID
			if store.YTArticleExcluded(d.Key, d.TaskID) {
				continue
			}
			out.Documents++
			p := byProject[d.Bucket]
			if p == nil {
				p = &ytAuditDocProject{Project: d.Bucket, Unbound: ytArticlesClass{DocumentIDs: []int64{}}}
				byProject[d.Bucket] = p
				out.Projects = append(out.Projects, p)
			}
			if d.BoundTenant != a.tenant {
				out.Unbound.add(d.DocumentID)
				p.Unbound.add(d.DocumentID)
			}
		}
		if len(docs) < ytArticleDocPage {
			break
		}
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Project < out.Projects[j].Project })
	return out, nil
}

// gap reports whether the run found what the cutover gate refuses.
func (r *ytAuditReport) gap() bool {
	return r.Total.MissingInYT.Count > 0 || r.Total.Duplicate.Count > 0 || r.Total.AdoptedUnmatched.Count > 0 || r.Total.AmbiguousCopy.Count > 0 || r.Total.Unbound.Count > 0
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
	// toPG: comments are canonical in YouTrack (ytAuditRepairToPG); the run
	// enqueues nothing, even with enqueue set.
	toPG bool
	// articles is set when the schema has yt_article_bindings: the run
	// then also counts the unbound documents (ytAuditDocuments).
	articles bool
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
		b, verr = youtrack.VerifyBinding(ctx, a.remote, a.tenant, "", c) // c has a project: no bucket
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
	// recorded link that is a YouTrack id, and (after op renderings, in
	// AuditComments) a comment ending in an hk comment's legacy marker.
	for i, c := range hk.Comments {
		if link := hk.Links[c.ID]; youtrack.CommentLinkIsID(link) {
			hk.Comments[i].LinkedYTIDs = append(hk.Comments[i].LinkedYTIDs, link)
		}
	}
	got := ytwrite.AuditComments(hk.Comments, yt, a.botID, youtrack.LegacyCommentID)
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
	bodyByCommentID := make(map[int64]string, len(hk.Comments))
	for _, c := range hk.Comments {
		bodyByCommentID[c.ID] = c.Body
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
		for _, x := range got.AmbiguousCopy {
			c.AmbiguousCopy.add(ytAuditItem{TaskID: taskID, CommentIDs: x.CommentIDs, YTIDs: []string{x.YTID}})
		}
		for _, id := range got.DocComment {
			c.DocComment.add(ytAuditItem{TaskID: taskID, YTIDs: []string{id}})
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
		if strings.TrimSpace(bodyByCommentID[x.CommentID]) == "" {
			continue
		}
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
		case status == store.YTAuditAuthority:
			// Comments switched to YouTrack during the run: stop
			// enqueuing; the report's direction follows (run).
			enq.Skipped++
			a.toPG, a.enqueue = true, false
			return nil
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
	r := ytAuditReport{Tenant: a.tenant, RepairDirection: ytAuditRepairToYT, Project: project, Task: taskID, Limit: limit, Projects: []*ytAuditProject{}}
	if a.toPG {
		r.RepairDirection = ytAuditRepairToPG
		if a.enqueue {
			a.enqueue = false
			r.EnqueueSkipped = "comments authority is youtrack: repair from YouTrack to PG; no op is prepared"
		}
	}
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
	if a.toPG && r.RepairDirection != ytAuditRepairToPG {
		r.RepairDirection = ytAuditRepairToPG
		r.EnqueueSkipped = "comments authority switched to youtrack during the run: enqueuing stopped"
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Project < r.Projects[j].Project })
	for _, p := range append([]*ytAuditCounts{&r.Total}, projectCounts(r.Projects)...) {
		p.fillEmpty()
	}
	if a.articles && taskID == 0 {
		docs, err := a.documents(ctx, project)
		if err != nil {
			return r, err
		}
		r.Documents = docs
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
	for _, x := range []*ytAuditClass{&c.Present, &c.MissingInYT, &c.Duplicate, &c.AdoptedUnmatched, &c.AmbiguousCopy, &c.DocComment, &c.YTOnlyBot, &c.YTOnlyHuman, &c.Error,
		&c.Unbound.NoProject, &c.Unbound.NoIssue, &c.Unbound.Ambiguous, &c.Unbound.Verifies, &c.Unbound.OtherTenant} {
		if x.Items == nil {
			x.Items = []ytAuditItem{}
		}
	}
}

const youtrackAuditUsage = "usage: youtrack audit [--project P] [--task N] [--enqueue] [--limit N] | " + youtrackRebuildUsage

// isRebuildFlag reports whether arg turns on the restore drill (--rebuild,
// youtrackRebuildCmd), which takes its own flags.
func isRebuildFlag(arg string) bool {
	switch arg {
	case "--rebuild", "-rebuild", "--rebuild=true", "-rebuild=true":
		return true
	}
	return false
}

// youtrackAuditCmd is `youtrack audit`. It opens the database directly and
// reads YouTrack with the bot key, like comments-backfill, against the tenant
// the comment worker uses (the host of YOUTRACK_URL). Without --enqueue it
// writes nothing (store.OpenReadOnly). The report goes to stdout; then a
// task whose reads failed, a stopped run or a failed enqueue exits 1, and a
// missing_in_yt or duplicate comment or an unbound task exits
// youtrackExitAuditGap.
func youtrackAuditCmd(args []string, out io.Writer) error {
	if slices.ContainsFunc(args, isRebuildFlag) {
		return youtrackRebuildCmd(args, out)
	}
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
		for _, id := range ytBindingMigrations {
			applied, err := st.MigrationApplied(openCtx, id)
			if err != nil {
				return err
			}
			if !applied {
				return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the audit never migrates", id)}
			}
		}
	}
	// The document count needs yt_article_bindings; a schema from before
	// it (read-only, so not migrated) is audited without the count.
	articles, err := st.MigrationApplied(openCtx, store.YTArticleBindingsMigration)
	if err != nil {
		return err
	}
	me, err := client.Me(openCtx)
	if err != nil {
		return fmt.Errorf("read the bot's YouTrack user: %w", err)
	}
	toPG, err := ytAuditToPG(openCtx, st)
	if err != nil {
		return err
	}
	a := &ytAuditor{st: st, remote: remote, tenant: remote.Tenant(), botID: me.ID, enqueue: *enqueue, toPG: toPG, articles: articles}
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
		return exitCodeError{code: youtrackExitAuditGap, err: fmt.Errorf("audit gap: %d missing_in_yt, %d duplicate, %d adopted_unmatched, %d ambiguous_copy, %d unbound",
			report.Total.MissingInYT.Count, report.Total.Duplicate.Count, report.Total.AdoptedUnmatched.Count, report.Total.AmbiguousCopy.Count, report.Total.Unbound.Count)}
	}
	return nil
}

// ytAuditToPG reads whether comments are canonical in YouTrack. A database
// without the authority table (an older schema the read-only audit does not
// migrate) is hk.
func ytAuditToPG(ctx context.Context, st *store.Store) (bool, error) {
	applied, err := st.MigrationApplied(ctx, store.YTAuthorityMigration)
	if err != nil || !applied {
		return false, err
	}
	side, err := st.YTAuthorityOf(ctx, store.YTAuthorityComments)
	return side == store.YTAuthorityYouTrack, err
}

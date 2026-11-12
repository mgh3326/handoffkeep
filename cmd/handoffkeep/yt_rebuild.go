package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `handoffkeep youtrack audit --rebuild` (hk 4121, the restore drill;
// docs/youtrack-write-protocol.md §Rebuild audit) shows that YouTrack's text
// and PG's id maps alone rebuild what hk needs: for each target it rebuilds
// the hk value from YouTrack reads and the binding tables, and diffs it with
// PG. It is the C3 gate of the switch (hk 3833 §1.2, §3.3). Read-only: it
// opens the store with OpenReadOnly and only GETs YouTrack.

const (
	// ytRebuildSampleDefault is how many random bound tasks have their
	// comments compared by default.
	ytRebuildSampleDefault = 200
	// ytRebuildTopComments is how many tasks with the most comments are
	// compared besides the random ones.
	ytRebuildTopComments = 20
)

// ytRebuildItem is one listed id and a one-line reason.
type ytRebuildItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ytRebuildClass is a count and its first ytAuditSample items.
type ytRebuildClass struct {
	Count int             `json:"count"`
	Items []ytRebuildItem `json:"items"`
}

func (c *ytRebuildClass) add(id, reason string) {
	c.Count++
	if len(c.Items) < ytAuditSample {
		c.Items = append(c.Items, ytRebuildItem{ID: id, Reason: reason})
	}
}

// ytRebuildTarget is one target's result. Checked = Equal + Differ.Count +
// Exceptions.Count; an item whose reads failed is in Errors and not checked.
type ytRebuildTarget struct {
	Checked    int            `json:"checked"`
	Equal      int            `json:"equal"`
	Differ     ytRebuildClass `json:"differ"`
	Exceptions ytRebuildClass `json:"exceptions"`
	Errors     ytRebuildClass `json:"errors"`
	// Skipped, when set, says why the target did not run (and then nothing
	// was checked); SkippedCount is how many it would have checked.
	Skipped      string `json:"skipped,omitempty"`
	SkippedCount int    `json:"skipped_count,omitempty"`
	// NotRebuildable is what of this target stays PG-only (hk 3833 §1.5):
	// listed so that a clean result is not read as covering it.
	NotRebuildable []string `json:"not_rebuildable"`
}

func (t *ytRebuildTarget) equal() { t.Checked++; t.Equal++ }

func (t *ytRebuildTarget) differ(id, reason string) { t.Checked++; t.Differ.add(id, reason) }

func (t *ytRebuildTarget) exception(id, reason string) { t.Checked++; t.Exceptions.add(id, reason) }

// fillEmpty makes every empty item list [] rather than null.
func (t *ytRebuildTarget) fillEmpty() {
	for _, c := range []*ytRebuildClass{&t.Differ, &t.Exceptions, &t.Errors} {
		if c.Items == nil {
			c.Items = []ytRebuildItem{}
		}
	}
	if t.NotRebuildable == nil {
		t.NotRebuildable = []string{}
	}
}

// ytRebuildComments is the comments target: the unit checked is one hk
// comment, or one YouTrack comment that copies none.
type ytRebuildComments struct {
	ytRebuildTarget
	// Tasks is how many sampled tasks were read (TopTasks of them for their
	// comment count).
	Tasks    int `json:"tasks"`
	TopTasks int `json:"top_tasks"`
	// PulledLeftOut is the comments the activity pull copied from YouTrack
	// into hk, left out on both sides; OtherHKWrites the YouTrack comments
	// hk wrote that are not one of the task's comments (a task-tied
	// document's copy, for one), left out too.
	PulledLeftOut int `json:"pulled_left_out"`
	OtherHKWrites int `json:"other_hk_writes"`
	// DocumentCopies is the import's copies of the task's reports and
	// briefs (ytwrite.DocComments of the document as it is now), left out:
	// they are documents, not task comments.
	DocumentCopies int `json:"document_copies"`
}

type ytRebuildReport struct {
	Tenant  string `json:"tenant"`
	Project string `json:"project,omitempty"`
	// Seed picks the random comment sample; the same seed on the same data
	// picks the same tasks (--seed).
	Seed           int64  `json:"seed"`
	SampleComments int    `json:"sample_comments"`
	WallMS         int64  `json:"wall_ms"`
	Wall           string `json:"wall"`
	// Stopped, when set, says why the run ended early: a rate limit longer
	// than the drill waits.
	Stopped   string            `json:"stopped,omitempty"`
	Documents ytRebuildTarget   `json:"documents"`
	Comments  ytRebuildComments `json:"comments"`
	Tasks     ytRebuildTarget   `json:"tasks"`
	Decisions ytRebuildTarget   `json:"decisions"`
}

func (r *ytRebuildReport) targets() []struct {
	name string
	t    *ytRebuildTarget
} {
	return []struct {
		name string
		t    *ytRebuildTarget
	}{{"documents", &r.Documents}, {"comments", &r.Comments.ytRebuildTarget}, {"tasks", &r.Tasks}, {"decisions", &r.Decisions}}
}

func (r *ytRebuildReport) differs() int {
	n := 0
	for _, x := range r.targets() {
		n += x.t.Differ.Count
	}
	return n
}

func (r *ytRebuildReport) errors() int {
	n := 0
	for _, x := range r.targets() {
		n += x.t.Errors.Count
	}
	return n
}

// What stays PG-only, per target (hk 3833 §1.5).
var (
	ytRebuildTasksPGOnly = []string{
		"state, claim and lease (claimed_by, refs.job_id, job lock) and fencing",
		"task_events and the sync_log change feed",
		"labels, relations and parent (the parent is a description line only)",
		"translations, standing orders, status updates",
		"the task id sequence and yt_task_bindings themselves",
	}
	ytRebuildCommentsPGOnly = []string{
		"the hk id of an imported comment (matched by task, time, author and body; no id map)",
		"comments the activity pull copied from YouTrack (YouTrack's own, left out)",
		"tasks outside the sample",
	}
	ytRebuildDocumentsPGOnly = []string{
		"screens/* and review/mwr-* documents (hk only by operator decision)",
		"task reports, which are comments on their task's issue",
		"documents over 240 KiB until the document kind rewrites them (exception_not_updated, D4)",
		"attachment metadata",
	}
	ytRebuildDecisionsPGOnly = []string{
		"decision_answer_attempts and decision_resolutions",
		"option ids, question revisions and hashes, delivery records",
	}
)

// ytRebuildSource is the PG side of the drill (*store.Store).
type ytRebuildSource interface {
	YTRebuildTasks(ctx context.Context, tenant, project string, afterID int64, limit int) ([]store.YTRebuildTask, error)
	YTRebuildSampleTasks(ctx context.Context, tenant, project string, seed int64, n, top int) ([]store.YTRebuildSample, error)
	YTRebuildTaskComments(ctx context.Context, taskID int64, issueID string) (store.YTRebuildComments, error)
	YTRebuildDocuments(ctx context.Context, tenant, project string, afterID int64, limit int) ([]store.YTRebuildDocument, error)
	YTRebuildDecisions(ctx context.Context, tenant string, afterID int64, limit int) ([]store.YTRebuildDecision, error)
	YTRebuildCount(ctx context.Context, what, project string) (int, error)
}

// ytRebuildRemote is the YouTrack the drill reads.
type ytRebuildRemote interface {
	youtrack.IssueReader
	ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error)
	ArticleContent(ctx context.Context, articleID string) (string, error)
}

// ytRebuilder runs one drill, one remote read at a time; each read waits out
// a 429 as the comment audit does (ytAuditor.call).
type ytRebuilder struct {
	src     ytRebuildSource
	remote  ytRebuildRemote
	reader  *ytAuditor
	tenant  string
	project string
	// taskPage is how many bound tasks one page lists (a test pages a few).
	taskPage int
	// articles: the schema has yt_article_bindings (hk 4053); without it
	// the documents target is skipped.
	articles bool
	// decisions: the schema has yt_decision_bindings (hk 4054); without it
	// the decisions target is skipped with a count.
	haveDecisions bool
}

// ytRebuildArticleBindingsMigration adds yt_article_bindings (hk 4053).
const ytRebuildArticleBindingsMigration = "20261112-yt-article-bindings"

func (b *ytRebuilder) call(ctx context.Context, read func(context.Context) error) error {
	return b.reader.call(ctx, read)
}

// stopped reports whether err ends the run: a rate limit longer than the
// drill waits, or the caller's context.
func stopped(ctx context.Context, err error) bool {
	var stop errYTAuditStop
	return errors.As(err, &stop) || ctx.Err() != nil
}

// tasks rebuilds every bound task's id, title and project from its issue.
func (b *ytRebuilder) tasks(ctx context.Context, out *ytRebuildTarget) error {
	page := b.taskPage
	if page < 1 {
		page = store.YTRebuildTaskPageMax
	}
	var after int64
	for {
		tasks, err := b.src.YTRebuildTasks(ctx, b.tenant, b.project, after, page)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			after = t.TaskID
			id := strconv.FormatInt(t.TaskID, 10)
			var issue youtrack.Issue
			err := b.call(ctx, func(ctx context.Context) error {
				var gerr error
				issue, gerr = b.remote.GetIssue(ctx, t.IssueID)
				return gerr
			})
			switch {
			case errors.Is(err, youtrack.ErrNotFound):
				out.differ(id, "issue "+t.IssueID+" not found")
				continue
			case err != nil:
				out.Errors.add(id, err.Error())
				log.Printf("youtrack.rebuild_task_failed task_id=%d err=%q", t.TaskID, err.Error())
				if stopped(ctx, err) {
					return err
				}
				continue
			}
			if reason := diffTask(t, issue); reason != "" {
				out.differ(id, reason)
			} else {
				out.equal()
			}
		}
		if len(tasks) < page {
			return nil
		}
	}
}

// documents compares every bound document's article content with the
// document: its content sha256 must be documents.sha256. A document whose
// binding names the import's split index article (over 240 KiB when
// imported) is listed as exception_not_updated (D4, operator A53): its
// articles are the import's and are not updated at the first switch. Whether
// it was split is the binding's record, not the current body size: a
// document that grew past 240 KiB after a single-part import is compared.
func (b *ytRebuilder) documents(ctx context.Context, out *ytRebuildTarget) error {
	page := b.taskPage
	if page < 1 {
		page = store.YTRebuildTaskPageMax
	}
	var after int64
	for {
		docs, err := b.src.YTRebuildDocuments(ctx, b.tenant, b.project, after, page)
		if err != nil {
			return err
		}
		for _, d := range docs {
			after = d.DocumentID
			id := strconv.FormatInt(d.DocumentID, 10)
			if d.SplitIndex {
				out.exception(id, fmt.Sprintf("exception_not_updated: %s (%d bytes) is bound to the import's split index article %s (D4)", d.Key, d.BodyBytes, d.ArticleID))
				continue
			}
			var content string
			err := b.call(ctx, func(ctx context.Context) error {
				var rerr error
				content, rerr = b.remote.ArticleContent(ctx, d.ArticleID)
				return rerr
			})
			switch {
			case errors.Is(err, youtrack.ErrNotFound):
				out.differ(id, "article "+d.ArticleID+" not found")
				continue
			case err != nil:
				out.Errors.add(id, err.Error())
				log.Printf("youtrack.rebuild_document_failed document_id=%d err=%q", d.DocumentID, err.Error())
				if stopped(ctx, err) {
					return err
				}
				continue
			}
			sum := sha256.Sum256([]byte(content))
			if got := hex.EncodeToString(sum[:]); got != d.SHA256 {
				out.differ(id, fmt.Sprintf("article %s content sha256 %.12s, hk %.12s", d.ArticleID, got, d.SHA256))
				continue
			}
			out.equal()
		}
		if len(docs) < page {
			return nil
		}
	}
}

// decisions rebuilds every bound decision's question, options and answer
// from its DEC issue. A decision is not filtered by --project: DEC issues
// live in one project of their own.
func (b *ytRebuilder) decisions(ctx context.Context, out *ytRebuildTarget) error {
	page := b.taskPage
	if page < 1 {
		page = store.YTRebuildTaskPageMax
	}
	var after int64
	for {
		decisions, err := b.src.YTRebuildDecisions(ctx, b.tenant, after, page)
		if err != nil {
			return err
		}
		for _, d := range decisions {
			after = d.DecisionID
			id := "D-" + strconv.FormatInt(d.DecisionID, 10)
			if d.Deleted {
				out.differ(id, "bound to issue "+d.IssueID+" but deleted in hk")
				continue
			}
			var issue youtrack.Issue
			err := b.call(ctx, func(ctx context.Context) error {
				var gerr error
				issue, gerr = b.remote.GetIssue(ctx, d.IssueID)
				return gerr
			})
			switch {
			case errors.Is(err, youtrack.ErrNotFound):
				out.differ(id, "issue "+d.IssueID+" not found")
				continue
			case err != nil:
				out.Errors.add(id, err.Error())
				log.Printf("youtrack.rebuild_decision_failed decision_id=%d err=%q", d.DecisionID, err.Error())
				if stopped(ctx, err) {
					return err
				}
				continue
			}
			if reason := diffDecision(d.Text, issue.Description); reason != "" {
				out.differ(id, reason)
			} else {
				out.equal()
			}
		}
		if len(decisions) < page {
			return nil
		}
	}
}

// rebuildDecision reads a DEC issue's description in the Y6 form: the
// RenderDecision block alone (a decision_fields snapshot), or between
// decision_create's provenance line and its marker (RenderDecisionCreate).
// The import's description has no options or question block and is not
// read.
func rebuildDecision(description string) (ytwrite.DecisionText, bool) {
	if x, ok := ytwrite.ParseDecision(description); ok {
		return x, true
	}
	first, rest, found := strings.Cut(description, "\n")
	if !found {
		return ytwrite.DecisionText{}, false
	}
	_, _, opID, ok := ytwrite.ParseProvenance(first)
	if !ok {
		return ytwrite.DecisionText{}, false
	}
	block, ok := strings.CutSuffix(rest, "\n\n"+ytwrite.OpMarker(opID))
	if !ok {
		return ytwrite.DecisionText{}, false
	}
	return ytwrite.ParseDecision(block)
}

// diffDecision is "" when the description rebuilds want's question, options
// and answer, else the first difference.
func diffDecision(want ytwrite.DecisionText, description string) string {
	x, ok := rebuildDecision(description)
	if !ok {
		return "description is not in the Y6 decision form"
	}
	want = ytwrite.CanonicalDecision(want)
	switch {
	case x.DecisionID != want.DecisionID:
		return fmt.Sprintf("description names D-%d", x.DecisionID)
	case x.Question != want.Question:
		return fmt.Sprintf("question %q", truncateRunes(x.Question, 80))
	case !slices.Equal(x.Options, want.Options):
		return fmt.Sprintf("options %v, hk %v", x.Options, want.Options)
	case x.Answer != want.Answer:
		return fmt.Sprintf("answer %q, hk %q", truncateRunes(x.Answer, 80), truncateRunes(want.Answer, 80))
	}
	return ""
}

// ytRebuiltTask is what an issue says the task is.
type ytRebuiltTask struct {
	ID      int64
	Title   string
	Project string
	// OneLine: the title was written with whitespace collapsed
	// (ytwrite.RenderFields), so PG's title is compared the same way.
	OneLine bool
}

// importTitlePrefix opens the description the import wrote
// (youtrack/import/client.js issue()).
const importTitlePrefix = "Original hk title: "

// rebuildTask reads an issue as hk would rebuild its task. The hk ID field
// gives the id (else the description's task line); the description's title
// line the title, in whichever of the three forms wrote it (issue_fields'
// RenderFields, issue_create's RenderIssue, the import's "Original hk
// title:"); the hk 프로젝트 field the project (else the description's
// project), with Unassigned read as no project. reason says what could not be
// read.
func rebuildTask(issue youtrack.Issue) (x ytRebuiltTask, reason string) {
	descProject, haveDescProject := "", false
	var descID int64
	if f, ok := ytwrite.ParseFields(issue.Description); ok {
		x.Title, x.OneLine = f.Title, true
		descID, descProject, haveDescProject = f.TaskID, f.Project, true
	} else if f, ok := ytwrite.ParseIssue(issue.Description); ok {
		x.Title = f.Title
		descID, descProject, haveDescProject = f.TaskID, f.Project, true
	} else if rest, ok := strings.CutPrefix(issue.Description, importTitlePrefix); ok {
		x.Title, _, _ = strings.Cut(rest, "\n")
	} else {
		return x, "description has no hk title line"
	}
	if f, ok := youtrack.IssueFieldByName(issue.CustomFields, ytwrite.FieldHKID); ok {
		if n, ok := f.IntValue(); ok {
			x.ID = int64(n)
		}
	}
	if x.ID == 0 {
		x.ID = descID
	}
	if x.ID == 0 {
		return x, "no hk ID field and no task line"
	}
	x.Project = descProject
	if f, ok := youtrack.IssueFieldByName(issue.CustomFields, ytwrite.FieldProject); ok && f.StringValue() != "" {
		v := f.StringValue()
		if haveDescProject && v != descProject {
			return x, fmt.Sprintf("hk 프로젝트 %q but description project %q", v, descProject)
		}
		x.Project, haveDescProject = v, true
	}
	if !haveDescProject {
		return x, "no hk 프로젝트 field and no project line"
	}
	if x.Project == ytwrite.UnassignedProject {
		x.Project = ""
	}
	return x, ""
}

// diffTask is "" when the issue rebuilds t, else the first difference.
func diffTask(t store.YTRebuildTask, issue youtrack.Issue) string {
	x, reason := rebuildTask(issue)
	if reason != "" {
		return reason
	}
	want := t.Title
	if x.OneLine {
		want = strings.Join(strings.Fields(want), " ")
	}
	switch {
	case x.ID != t.TaskID:
		return fmt.Sprintf("hk ID %d", x.ID)
	case x.Title != want:
		return fmt.Sprintf("title %q", truncateRunes(x.Title, 80))
	case x.Project != t.Project:
		return fmt.Sprintf("project %q, hk %q", x.Project, t.Project)
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// comments compares the sampled tasks' comments.
func (b *ytRebuilder) comments(ctx context.Context, seed int64, n int, out *ytRebuildComments) error {
	sample, err := b.src.YTRebuildSampleTasks(ctx, b.tenant, b.project, seed, n, ytRebuildTopComments)
	if err != nil {
		return err
	}
	for _, t := range sample {
		out.Tasks++
		if t.Top {
			out.TopTasks++
		}
		id := strconv.FormatInt(t.TaskID, 10)
		hk, err := b.src.YTRebuildTaskComments(ctx, t.TaskID, t.IssueID)
		if err != nil {
			return err
		}
		var listed []ytwrite.RemoteComment
		err = b.call(ctx, func(ctx context.Context) error {
			var lerr error
			listed, lerr = b.remote.ListComments(ctx, t.IssueID, ytAuditPageSize)
			return lerr
		})
		if err != nil {
			out.Errors.add("task "+id, err.Error())
			log.Printf("youtrack.rebuild_comments_failed task_id=%d err=%q", t.TaskID, err.Error())
			if stopped(ctx, err) {
				return err
			}
			continue
		}
		diffComments(t.TaskID, hk, listed, out)
	}
	return nil
}

// How a YouTrack comment is read back as an hk comment.
const (
	// rebuiltHKWritten: in the rendered form of an op of one of the task's
	// comments; author and time from the provenance line (second
	// precision).
	rebuiltHKWritten = iota
	// rebuiltImported: author (login or name) and created time (millisecond
	// precision) as YouTrack lists them; matched by that tuple and the body.
	rebuiltImported
	// rebuiltLegacy: the pre-protocol mirror push's copy (its recorded
	// YouTrack id, or its trailer); only the body is hk's
	// (youtrack.commentText shortens the author and writes the time to the
	// minute).
	rebuiltLegacy
)

// rebuiltComment is one YouTrack comment read as an hk comment.
type rebuiltComment struct {
	ytID string
	how  int
	// hk is the index of the hk comment it copies (-1: none yet).
	hk      int
	authors []string
	at      time.Time
	body    string
}

// legacyBody is the hk body of a legacy mirror copy:
// "**<who>** · <time>\n\n<body>\n\n<!-- hk-comment:N -->".
func legacyBody(text string) string {
	_, rest, _ := strings.Cut(text, "\n\n")
	if i := strings.LastIndex(rest, "\n\n<!-- hk-comment:"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// diffComments compares one task's hk comments with its issue's, as hk
// would rebuild them: an hk-written comment (ytwrite.RenderedOpID whose op is
// one of the task's comments' ops, through yt_comment_write_links) gives its
// author and time from the provenance line and its body from between that
// line and the marker; an imported one gives YouTrack's author and created
// time and its text, and is matched with the hk comment of the same (time,
// author, body); a legacy mirror copy gives only the body, so an equal body
// is an exception, not a pass. Every hk comment must have exactly one copy,
// and every YouTrack comment must copy one.
func diffComments(taskID int64, hk store.YTRebuildComments, listed []ytwrite.RemoteComment, out *ytRebuildComments) {
	byOp := map[int64]int{}
	byID := map[int64]int{}
	byLegacyYT := map[string]int{}
	// The import's copies of the task's documents, each expected once, by
	// (body, author, time) as the import wrote them: the document's creator
	// and creation time (youtrack/import/client.js documentIssueComments).
	var docCopies []store.YTRebuildComment
	for _, d := range hk.Docs {
		bodies, _, _ := ytwrite.DocComments(d.Key, d.Kind, d.Body)
		for _, b := range bodies {
			docCopies = append(docCopies, store.YTRebuildComment{Author: d.CreatedBy, Body: b, CreatedAt: d.CreatedAt})
		}
	}
	docUsed := make([]bool, len(docCopies))
	for i, c := range hk.Comments {
		byID[c.ID] = i
		if c.OpID > 0 {
			byOp[c.OpID] = i
		}
		if youtrack.CommentLinkIsID(c.LegacyYTID) {
			byLegacyYT[c.LegacyYTID] = i
		}
	}
	// The listing comes in no fixed order (EnumerateStableComments returns
	// a set); created time, then id, makes the matching and the report the
	// same on every run.
	listed = slices.Clone(listed)
	slices.SortFunc(listed, func(a, b ytwrite.RemoteComment) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	var rebuilt []rebuiltComment
	for _, c := range listed {
		if c.Deleted {
			continue
		}
		if hk.PulledYTIDs[c.ID] {
			out.PulledLeftOut++
			continue
		}
		if i, ok := byLegacyYT[c.ID]; ok {
			rebuilt = append(rebuilt, rebuiltComment{ytID: c.ID, how: rebuiltLegacy, hk: i, body: legacyBody(c.Text)})
			continue
		}
		if opID, ok := ytwrite.RenderedOpID(c.Text); ok {
			i, mine := byOp[opID]
			switch {
			case mine:
			case hk.IssueOps[opID]:
				// hk's own write to this issue, of another entity (a
				// task-tied document's copy, for one).
				out.OtherHKWrites++
				continue
			default:
				// The form alone is not hk's: no op of hk's ledger for this
				// issue has the id.
				out.differ("yt "+c.ID, "task "+strconv.FormatInt(taskID, 10)+": names hk op "+strconv.FormatInt(opID, 10)+", which hk's ledger has not for this issue")
				continue
			}
			first, rest, _ := strings.Cut(c.Text, "\n\n")
			actor, at, _, _ := ytwrite.ParseProvenance(first)
			body := strings.TrimSuffix(rest, "\n\n"+ytwrite.OpMarker(opID))
			rebuilt = append(rebuilt, rebuiltComment{ytID: c.ID, how: rebuiltHKWritten, hk: i, authors: []string{actor}, at: at, body: body})
			continue
		}
		if n, ok := youtrack.LegacyCommentID(c.Text); ok {
			if i, ok := byID[n]; ok {
				rebuilt = append(rebuilt, rebuiltComment{ytID: c.ID, how: rebuiltLegacy, hk: i, body: legacyBody(c.Text)})
				continue
			}
		}
		rebuilt = append(rebuilt, rebuiltComment{ytID: c.ID, how: rebuiltImported, hk: -1,
			authors: []string{c.AuthorLogin, c.AuthorName}, at: c.CreatedAt, body: c.Text})
	}
	copies := make([][]int, len(hk.Comments))
	// The id-mapped copies first, so a tuple match never takes their hk
	// comment.
	for j, r := range rebuilt {
		if r.hk >= 0 {
			copies[r.hk] = append(copies[r.hk], j)
		}
	}
	task := strconv.FormatInt(taskID, 10)
	for j := range rebuilt {
		r := &rebuilt[j]
		if r.how != rebuiltImported {
			continue
		}
		// An expected document copy first, consumed once, so a task comment
		// with the same text is still matched and an extra copy is not
		// hidden.
		doc := -1
		for k, d := range docCopies {
			if !docUsed[k] && importedMatches(*r, d) {
				doc = k
				break
			}
		}
		if doc >= 0 {
			docUsed[doc] = true
			out.DocumentCopies++
			continue
		}
		// An hk comment with no copy yet first; else one that has a copy
		// already, which makes this one its duplicate.
		for _, free := range []bool{true, false} {
			for i, c := range hk.Comments {
				if r.hk < 0 && (len(copies[i]) == 0) == free && importedMatches(*r, c) {
					r.hk = i
					copies[i] = append(copies[i], j)
				}
			}
		}
		switch key := docCopyKey(r.body, hk.Docs); {
		case r.hk >= 0:
		case key != "":
			out.differ("yt "+r.ytID, "task "+task+": names document "+key+" but is no expected copy of it (an extra copy, or the document changed; body, author, time)")
		default:
			out.differ("yt "+r.ytID, "task "+task+": YouTrack comment matches no hk comment (author, time, body)")
		}
	}
	for i, c := range hk.Comments {
		id := "comment " + strconv.FormatInt(c.ID, 10)
		if len(copies[i]) != 1 {
			reason := "not in YouTrack"
			if len(copies[i]) > 1 {
				ids := []string{}
				for _, j := range copies[i] {
					ids = append(ids, rebuilt[j].ytID)
				}
				reason = strconv.Itoa(len(ids)) + " copies " + strings.Join(ids, ",")
			}
			out.differ(id, "task "+task+": "+reason)
			continue
		}
		r := rebuilt[copies[i][0]]
		switch {
		case r.how == rebuiltLegacy && r.body == c.Body:
			out.exception(id, "legacy_mirror: "+r.ytID+" is the pre-protocol mirror copy; body equal, author and time not in YouTrack")
		case r.how == rebuiltLegacy:
			out.differ(id, "task "+task+": legacy mirror copy "+r.ytID+" body differs")
		case r.body != c.Body:
			out.differ(id, "task "+task+": "+r.ytID+" body differs")
		case r.how == rebuiltImported:
			out.equal() // matched by the whole tuple
		case r.authors[0] == c.Author && r.at.Equal(c.CreatedAt.Truncate(time.Second)):
			out.equal()
		case r.authors[0] == c.Author && r.at.Equal(c.OpCreatedAt.Truncate(time.Second)):
			// A catch-up op (CatchUpYTComments) is made after the comment and
			// its provenance names its own time: the comment's time is not in
			// YouTrack.
			out.exception(id, fmt.Sprintf("op_time: %s names the time of op %d (%s), not the comment's (%s); the comment's time is PG-only",
				r.ytID, c.OpID, r.at.Format(time.RFC3339), c.CreatedAt.Format(time.RFC3339Nano)))
		default:
			out.differ(id, fmt.Sprintf("task %s: %s provenance %q at %s, hk %q at %s", task, r.ytID,
				r.authors[0], r.at.Format(time.RFC3339), c.Author, c.CreatedAt.Format(time.RFC3339Nano)))
		}
	}
}

// docCopyKey is the key of the task document an import copy names (a report's
// "# <key>" heading, or a brief's "(hk document key: <key>)" line), "" when
// the text names none of the task's documents.
func docCopyKey(text string, docs []store.YTRebuildTaskDoc) string {
	for _, d := range docs {
		if _, ok, _ := ytwrite.DocComments(d.Key, d.Kind, ""); !ok {
			continue
		}
		heading := "# " + d.Key
		if text == heading || strings.HasPrefix(text, heading+"\n") || strings.HasPrefix(text, heading+" (part ") ||
			(strings.HasPrefix(text, "Article: ") && strings.HasSuffix(text, "(hk document key: "+d.Key+")")) {
			return d.Key
		}
	}
	return ""
}

// importedMatches reports whether an imported YouTrack comment is hk comment
// c: the same body, the same time to the millisecond (the import sent
// millisecond precision), and an author whose login or name is c's author
// (the import made each hk actor a user of that name).
func importedMatches(r rebuiltComment, c store.YTRebuildComment) bool {
	if r.body != c.Body || !r.at.Equal(c.CreatedAt.Truncate(time.Millisecond)) {
		return false
	}
	for _, a := range r.authors {
		if a != "" && a == c.Author {
			return true
		}
	}
	return false
}

// run runs every target. A rate limit longer than the drill waits stops the
// run (Stopped); any other failed read is one item's error.
func (b *ytRebuilder) run(ctx context.Context, seed int64, sample int) (ytRebuildReport, error) {
	start := time.Now()
	r := ytRebuildReport{Tenant: b.tenant, Project: b.project, Seed: seed, SampleComments: sample}
	r.Tasks.NotRebuildable = ytRebuildTasksPGOnly
	r.Comments.NotRebuildable = ytRebuildCommentsPGOnly
	r.Documents.NotRebuildable = ytRebuildDocumentsPGOnly
	r.Decisions.NotRebuildable = ytRebuildDecisionsPGOnly
	steps := []func() error{
		func() error {
			if b.articles {
				return b.documents(ctx, &r.Documents)
			}
			n, err := b.src.YTRebuildCount(ctx, "documents", b.project)
			r.Documents.Skipped, r.Documents.SkippedCount = "no yt_article_bindings in this schema (migration "+ytRebuildArticleBindingsMigration+")", n
			return err
		},
		func() error { return b.tasks(ctx, &r.Tasks) },
		func() error {
			if b.haveDecisions {
				return b.decisions(ctx, &r.Decisions)
			}
			n, err := b.src.YTRebuildCount(ctx, "decisions", b.project)
			r.Decisions.Skipped, r.Decisions.SkippedCount = "no yt_decision_bindings in this schema (migration "+store.YTDecisionBindingsMigration+")", n
			return err
		},
		func() error { return b.comments(ctx, seed, sample, &r.Comments) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			if !stopped(ctx, err) {
				return r, err
			}
			r.Stopped = err.Error()
			break
		}
	}
	for _, x := range r.targets() {
		x.t.fillEmpty()
	}
	wall := time.Since(start)
	r.WallMS, r.Wall = wall.Milliseconds(), wall.Round(time.Millisecond).String()
	return r, nil
}

// printRebuildText is the drill's text form: one line per target, then its
// listed items.
func printRebuildText(w io.Writer, r ytRebuildReport) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "youtrack audit --rebuild: tenant %s", r.Tenant)
	if r.Project != "" {
		fmt.Fprintf(&sb, ", project %s", r.Project)
	}
	fmt.Fprintf(&sb, ", seed %d, wall %s\n", r.Seed, r.Wall)
	if r.Stopped != "" {
		fmt.Fprintf(&sb, "stopped: %s\n", r.Stopped)
	}
	for _, x := range r.targets() {
		t := x.t
		if t.Skipped != "" {
			fmt.Fprintf(&sb, "%-9s skipped (%d not checked): %s\n", x.name, t.SkippedCount, t.Skipped)
		} else {
			fmt.Fprintf(&sb, "%-9s checked %d, equal %d, differ %d, exceptions %d, errors %d\n", x.name, t.Checked, t.Equal, t.Differ.Count, t.Exceptions.Count, t.Errors.Count)
		}
		for _, c := range []struct {
			name string
			c    ytRebuildClass
		}{{"differ", t.Differ}, {"exception", t.Exceptions}, {"error", t.Errors}} {
			for _, it := range c.c.Items {
				fmt.Fprintf(&sb, "  %s %s: %s\n", c.name, it.ID, it.Reason)
			}
		}
		fmt.Fprintf(&sb, "  not rebuildable: %s\n", strings.Join(t.NotRebuildable, "; "))
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

const youtrackRebuildUsage = "youtrack audit --rebuild [--project P] [--sample-comments N] [--seed S] [--json]"

// youtrackRebuildCmd is `youtrack audit --rebuild`. Like the comment audit it
// opens the database read-only and reads YouTrack with the bot key, against
// the tenant of YOUTRACK_URL. The report goes to stdout (text, or JSON with
// --json); then a failed read or a stopped run exits 1, and any differ exits
// youtrackExitAuditGap.
func youtrackRebuildCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack audit --rebuild", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("rebuild", true, "the restore drill")
	project := fs.String("project", "", "only this hk project's tasks, comments and documents")
	sample := fs.Int("sample-comments", ytRebuildSampleDefault, "random bound tasks whose comments are compared, besides the top 20 by comment count")
	seed := fs.Int64("seed", 0, "the random sample's seed (0: one from the clock, printed in the report)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	ordered, err := splitArgs(args, "project", "sample-comments", "seed")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; usage: %s", err, youtrackRebuildUsage)
	}
	if fs.NArg() != 0 || *sample < 0 || *sample > store.YTRebuildSampleMax {
		return errors.New("usage: " + youtrackRebuildUsage)
	}
	if *seed == 0 {
		*seed = time.Now().UnixNano()
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
	st, err := store.OpenReadOnly(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.MigrationApplied(openCtx, store.YTTaskBindingsMigration)
	if err != nil {
		return err
	}
	if !applied {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the drill never migrates", store.YTTaskBindingsMigration)}
	}
	articles, err := st.MigrationApplied(openCtx, ytRebuildArticleBindingsMigration)
	if err != nil {
		return err
	}
	decisions, err := st.MigrationApplied(openCtx, store.YTDecisionBindingsMigration)
	if err != nil {
		return err
	}
	b := &ytRebuilder{src: st, remote: remote, reader: &ytAuditor{}, tenant: remote.Tenant(), project: *project, articles: articles, haveDecisions: decisions}
	report, err := b.run(context.Background(), *seed, *sample)
	if err != nil {
		return err
	}
	if *asJSON {
		err = printJSON(out, report)
	} else {
		err = printRebuildText(out, report)
	}
	if err != nil {
		return err
	}
	switch {
	case report.errors() > 0 || report.Stopped != "":
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("the drill is incomplete (%d failed reads, stopped %q); run again", report.errors(), report.Stopped)}
	case report.differs() > 0:
		return exitCodeError{code: youtrackExitAuditGap, err: fmt.Errorf("rebuild differs: %d documents, %d comments, %d tasks, %d decisions",
			report.Documents.Differ.Count, report.Comments.Differ.Count, report.Tasks.Differ.Count, report.Decisions.Differ.Count)}
	}
	return nil
}

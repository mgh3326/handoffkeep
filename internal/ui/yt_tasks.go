package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// The console's task pages — the board, the work status page and the Home
// counts — read tasks from YouTrack, the system of record for tasks since
// 2026-11-12 (hk 4430, operator decision 212). The server reads YouTrack with
// the bot token it already holds; the browser never talks to YouTrack. hk
// still supplies what only hk knows (jobs, workers, PRs, spawn requests,
// decisions, inbox state), joined to each issue by task id. When YouTrack
// cannot be read the page answers 502 youtrack_unavailable: it never falls
// back to hk's own task copy.

// TaskIssueLister reads one page of task issues; *youtrack.Client is one.
type TaskIssueLister interface {
	ListTaskIssues(ctx context.Context, query string, skip, top int) ([]youtrack.Issue, error)
}

const (
	// defaultTaskIssuesTTL is how long one YouTrack read serves the pages.
	// YouTrack's issue list sends no ETag, so the cache is time-bound only.
	defaultTaskIssuesTTL = 15 * time.Second
)

// taskSourceYouTrack is the board's source field on the YouTrack-backed read.
const taskSourceYouTrack = "youtrack"

// errYouTrackRead marks a failure of YouTrack itself, as opposed to hk's
// database: the page answers 502 for it and 500 for the rest.
var errYouTrackRead = errors.New("youtrack_unavailable")

// ytTaskSource is the cached YouTrack read behind the task pages. One read
// runs at a time; callers that arrive during it wait for its result.
type ytTaskSource struct {
	issues TaskIssueLister
	store  ytTaskStore
	tenant string
	schema youtrack.Schema
	ttl    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	cached *ytSnapshot
}

// ytTaskStore is the part of the store the read needs: the mirrored
// projects and the issue → task links.
type ytTaskStore interface {
	ListYouTrackProjects(ctx context.Context) ([]store.YouTrackProject, error)
	YTIssueTaskIDs(ctx context.Context, tenant string) (map[string]store.YTIssueTask, error)
}

// ytSnapshot is every task issue of one read, one per task, in task id order.
type ytSnapshot struct {
	FetchedAt time.Time
	Tasks     []youtrack.TaskIssue
	byTask    map[int64]int
	// Unbound counts issues hk links to no task (no binding and no hk ID).
	Unbound int
}

func (s *ytSnapshot) task(id int64) (youtrack.TaskIssue, bool) {
	i, ok := s.byTask[id]
	if !ok {
		return youtrack.TaskIssue{}, false
	}
	return s.Tasks[i], true
}

// startedIDs are the tasks YouTrack shows in a started state: the only ones
// a stall can count for.
func (s *ytSnapshot) startedIDs() []int64 {
	ids := []int64{}
	for _, t := range s.Tasks {
		if t.StateKnown && ytStartedStates[t.State] {
			ids = append(ids, t.TaskID)
		}
	}
	return ids
}

func newYTTaskSource(issues TaskIssueLister, st ytTaskStore, tenant string, ttl time.Duration) *ytTaskSource {
	if ttl <= 0 {
		ttl = defaultTaskIssuesTTL
	}
	return &ytTaskSource{issues: issues, store: st, tenant: tenant, schema: youtrack.DefaultSchema(), ttl: ttl,
		now: func() time.Time { return time.Now().UTC() }}
}

// snapshot returns a read no older than the TTL. A failed read is not
// cached and an expired one is not served: the caller gets the error.
func (s *ytTaskSource) snapshot(ctx context.Context) (*ytSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cached != nil && now.Sub(s.cached.FetchedAt) < s.ttl {
		return s.cached, nil
	}
	snap, err := s.fetch(ctx, now)
	if err != nil {
		s.cached = nil
		return nil, err
	}
	s.cached = snap
	return snap, nil
}

func (s *ytTaskSource) fetch(ctx context.Context, now time.Time) (*ytSnapshot, error) {
	projects, err := s.store.ListYouTrackProjects(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{youtrack.BucketShortName}
	for _, p := range projects {
		names = append(names, p.ShortName)
	}
	query := youtrack.TaskIssuesQuery(names)
	var issues []youtrack.Issue
	for skip := 0; ; skip += youtrack.TaskIssuePageSize {
		page, err := s.issues.ListTaskIssues(ctx, query, skip, youtrack.TaskIssuePageSize)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errYouTrackRead, err)
		}
		issues = append(issues, page...)
		if len(page) < youtrack.TaskIssuePageSize {
			break
		}
	}
	links, err := s.store.YTIssueTaskIDs(ctx, s.tenant)
	if err != nil {
		return nil, err
	}
	return buildYTSnapshot(s.schema, issues, links, now), nil
}

// fieldLinkRank ranks an issue's own hk ID field after every link hk keeps.
const fieldLinkRank = store.YTLinkEntityMap + 1

// buildYTSnapshot maps each issue to its task: hk's link first (binding,
// mirror, entity map), then the issue's hk ID field. When two issues claim
// one task the stronger link wins, then the older issue.
func buildYTSnapshot(schema youtrack.Schema, issues []youtrack.Issue, links map[string]store.YTIssueTask, now time.Time) *ytSnapshot {
	type ranked struct {
		task youtrack.TaskIssue
		rank int
	}
	best := map[int64]ranked{}
	snap := &ytSnapshot{FetchedAt: now}
	for _, issue := range issues {
		t := youtrack.TaskFromIssue(schema, issue)
		rank := fieldLinkRank
		if link, ok := links[issue.ID]; ok {
			t.TaskID, rank = link.TaskID, link.Rank
		}
		if t.TaskID < 1 {
			snap.Unbound++
			continue
		}
		if have, ok := best[t.TaskID]; ok && have.rank <= rank {
			continue
		}
		best[t.TaskID] = ranked{t, rank}
	}
	snap.Tasks = make([]youtrack.TaskIssue, 0, len(best))
	for _, x := range best {
		snap.Tasks = append(snap.Tasks, x.task)
	}
	sort.Slice(snap.Tasks, func(i, j int) bool { return snap.Tasks[i].TaskID < snap.Tasks[j].TaskID })
	snap.byTask = make(map[int64]int, len(snap.Tasks))
	for i, t := range snap.Tasks {
		snap.byTask[t.TaskID] = i
	}
	return snap
}

// youTrackReadFailed answers a failed task-page read: 502 when YouTrack
// failed, 500 when hk's database did.
func youTrackReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("ui.youtrack_read_failed route=%q err=%q", r.Pattern, err)
	if errors.Is(err, errYouTrackRead) {
		writeJSONError(w, http.StatusBadGateway, "youtrack_unavailable")
		return
	}
	writeJSONError(w, http.StatusInternalServerError, "internal_error")
}

// withYouTrackFields puts the YouTrack-owned fields of an issue on hk's row of
// the same task. hk keeps the protocol fields: refs, claim, parent, decision
// flag, lifecycle timestamps.
func withYouTrackFields(task store.Task, issue youtrack.TaskIssue) store.Task {
	task.Title = issue.Title
	task.State = issue.State
	task.Category = store.TaskCategory(issue.State)
	task.Lane = issue.Lane
	task.Kind = issue.Kind
	task.Priority = issue.Priority
	task.Project = optionalText(issue.Project)
	task.Area = optionalText(issue.Area)
	return task
}

func optionalText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// ytStartedStates are the states a stalled task must be in.
var ytStartedStates = map[string]bool{store.StateResearching: true, store.StateDeveloping: true, store.StateReviewing: true}

// youTrackAttentionTasks keeps the waiting tasks YouTrack still shows open,
// with YouTrack's title, lane, state and priority. Why a task waits stays
// hk's: an open decision, or no activity for the stall window; a stalled
// task YouTrack shows outside the started states no longer counts.
func youTrackAttentionTasks(candidates []store.AttentionTask, snap *ytSnapshot) store.AttentionTasks {
	out := store.AttentionTasks{Items: []store.AttentionTask{}}
	for _, c := range candidates {
		issue, ok := snap.task(c.ID)
		if !ok || !issue.StateKnown || youtrack.StateResolved(issue.State) || issue.State == store.StateIcebox {
			continue
		}
		if c.Reason == store.AttentionReasonStalled && !ytStartedStates[issue.State] {
			continue
		}
		c.Title, c.Lane, c.State, c.Priority = issue.Title, issue.Lane, issue.State, issue.Priority
		if c.Reason == store.AttentionReasonAwaitingDecision {
			out.AwaitingDecision++
		} else {
			out.Stalled++
		}
		if len(out.Items) < store.AttentionItems {
			out.Items = append(out.Items, c)
		}
	}
	out.Count = out.AwaitingDecision + out.Stalled
	return out
}

// closeDays are the KST days a task closed on within the window: every hk
// transition into a closed state, whatever the task's state now, plus
// YouTrack's resolved time when the task is completed, dropped or duplicate
// in YouTrack and resolved after hk's last close (closed again in YouTrack
// with no hk event). A deployed issue's resolved time is its merge, and a
// resolved time before hk's last close belongs to an earlier lifecycle or to
// a resolved state before this one, so neither adds a day.
func closeDays(t youtrack.TaskIssue, events []time.Time, lastClosed time.Time, hasLast bool, start time.Time, kst *time.Location) map[string]bool {
	days := map[string]bool{}
	for _, at := range events {
		days[at.In(kst).Format("2006-01-02")] = true
	}
	ytClosed := t.StateKnown && (t.State == store.StateCompleted || t.State == store.StateDropped || t.State == store.StateDuplicate)
	if ytClosed && t.Resolved != nil && !t.Resolved.Before(start) && (!hasLast || t.Resolved.After(lastClosed)) {
		days[t.Resolved.In(kst).Format("2006-01-02")] = true
	}
	return days
}

// youTrackWorkStatus builds the work status document from YouTrack's tasks
// and hk's overlay. Movement counts, per KST day, issues created and the
// distinct YouTrack tasks that closed that day (closeDays).
func youTrackWorkStatus(now time.Time, snap *ytSnapshot, overlay store.WorkStatusOverlay) *store.WorkStatus {
	inSet := map[string]bool{}
	for _, s := range store.WorkStatusStates {
		inSet[s] = true
	}
	kst := time.FixedZone("KST", 9*3600)
	_, start := store.WorkStatusMovementStart(now)
	createdDays, closedDays := map[string]int{}, map[string]int{}
	var rows []store.WorkStatusRow
	for _, t := range snap.Tasks {
		bundle := strings.HasPrefix(t.Title, "[bundle ")
		if !bundle && !t.Created.IsZero() && !t.Created.Before(start) {
			createdDays[t.Created.In(kst).Format("2006-01-02")]++
		}
		if !bundle {
			last, hasLast := overlay.LastClosed[t.TaskID]
			for day := range closeDays(t, overlay.ClosedEvents[t.TaskID], last, hasLast, start, kst) {
				closedDays[day]++
			}
		}
		if !t.StateKnown || !inSet[t.State] {
			continue
		}
		row := store.WorkStatusRow{ID: t.TaskID, Title: t.Title, Project: optionalText(t.Project), State: t.State,
			Priority: t.Priority, CreatedAt: t.Created, UpdatedAt: t.Updated, JobID: t.Job, PR: t.PR}
		if x, ok := overlay.Tasks[t.TaskID]; ok {
			if x.JobID != "" {
				row.JobID = x.JobID
			}
			if x.PR != "" {
				row.PR = x.PR
			}
			row.Host, row.JobState = x.Host, x.JobState
			if x.ParentID != nil {
				row.ParentID, row.BundleID = x.ParentID, x.ParentID
				row.BundleTitle = x.ParentTitle
				if parent, ok := snap.task(*x.ParentID); ok {
					row.BundleTitle = parent.Title
				}
			}
			if at, ok := x.EnteredAt[t.State]; ok {
				row.InStateSince = &at
			}
			if at, ok := x.EnteredAt[store.StateMerged]; ok {
				row.MergedAt = &at
			}
		}
		// YouTrack resolves an issue when it merges. A later resolved time
		// than hk's last merge event is a merge hk has no event for (a
		// reopen merged again in YouTrack): the current merge is the later.
		if t.Resolved != nil && (row.MergedAt == nil || t.Resolved.After(*row.MergedAt)) {
			row.MergedAt = t.Resolved
		}
		rows = append(rows, row)
	}
	in := overlay.Inputs
	in.Movement = store.WorkStatusMovementDays(now, createdDays, closedDays)
	queued := make([]store.WorkStatusQueuedTask, 0, len(in.Queued))
	for _, q := range in.Queued {
		issue, ok := snap.task(q.ID)
		if !ok {
			continue
		}
		if issue.StateKnown && youtrack.StateResolved(issue.State) {
			continue
		}
		q.Title, q.State, q.Project = issue.Title, issue.State, optionalText(issue.Project)
		queued = append(queued, q)
	}
	in.Queued = queued
	return store.BuildWorkStatus(now, rows, in)
}

package youtrack

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// SyncOptions bound one run. The Dagu job runs every two minutes, so every
// list is a batch and one run never tries to catch up in a single pass.
type SyncOptions struct {
	DryRun        bool
	Pull          bool
	TaskLimit     int
	ActivityLimit int
	CommentLimit  int
	RefreshLimit  int
	RevertLimit   int
	OutboxLimit   int
	DocLimit      int
}

func (o SyncOptions) withDefaults() SyncOptions {
	if o.TaskLimit <= 0 {
		o.TaskLimit = 50
	}
	if o.ActivityLimit <= 0 {
		o.ActivityLimit = 100
	}
	if o.CommentLimit <= 0 {
		o.CommentLimit = 50
	}
	if o.RefreshLimit <= 0 {
		o.RefreshLimit = 100
	}
	if o.RevertLimit <= 0 {
		o.RevertLimit = 20
	}
	if o.OutboxLimit <= 0 {
		o.OutboxLimit = 100
	}
	if o.DocLimit <= 0 {
		o.DocLimit = 20
	}
	return o
}

type PushAction struct {
	TaskID     int64  `json:"task_id"`
	IssueID    string `json:"issue_id,omitempty"`
	ReadableID string `json:"readable_id,omitempty"`
	// Action is create, update, move, comment or unchanged.
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	CommentID int64  `json:"comment_id,omitempty"`
	Adopted   bool   `json:"adopted,omitempty"`
}

type PullAction struct {
	ActivityID string `json:"activity_id"`
	TaskID     int64  `json:"task_id,omitempty"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail,omitempty"`
}

type OutboxAction struct {
	TaskID  int64  `json:"task_id"`
	Op      string `json:"op"`
	Outcome string `json:"outcome"`
}

type SyncReport struct {
	DryRun    bool             `json:"dry_run"`
	Bot       string           `json:"bot,omitempty"`
	Outbox    []OutboxAction   `json:"outbox"`
	Pushed    []PushAction     `json:"push"`
	Pulled    []PullAction     `json:"pull"`
	Decisions []DecisionAction `json:"decisions"`
	Docs      []DocAction      `json:"docs"`
	DocBytes  int64            `json:"doc_bytes"`
	Refreshed int              `json:"refreshed"`
	Reverted  int              `json:"reverted"`
	Skipped   int              `json:"skipped"`
	Warnings  []string         `json:"warnings,omitempty"`
	Errors    []string         `json:"errors,omitempty"`
	Counts    map[string]int   `json:"counts"`
}

func (r *SyncReport) fail(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *SyncReport) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Sync runs one bounded pass: pending reverts, one page of YouTrack → hk
// activities, the priority/lane outbox, the field-cache refresh, the hk →
// YouTrack task push, the saved searches, and the DEC decision mirror. In dry-run mode nothing on either side is written.
func (s *Service) Sync(ctx context.Context, opts SyncOptions) (SyncReport, error) {
	opts = opts.withDefaults()
	report := SyncReport{DryRun: opts.DryRun, Outbox: []OutboxAction{}, Pushed: []PushAction{}, Pulled: []PullAction{}, Decisions: []DecisionAction{}, Docs: []DocAction{}, Counts: map[string]int{}}
	me, err := s.client.Me(ctx)
	if err != nil {
		return report, fmt.Errorf("youtrack me: %w", err)
	}
	if me.ID == "" && me.Login == "" {
		return report, fmt.Errorf("youtrack me: empty identity")
	}
	report.Bot = me.Login
	if me.Login == "" {
		report.Bot = me.ID
	}

	// When enabled, YouTrack -> hk runs first. The pull applies the operator's latest change to
	// PG; the outbox and the push then send the hk execution value. The other
	// order can overwrite YouTrack with an hk value the operator has already
	// changed, and this run would then apply their older activity and leave
	// the two sides inverted.
	if opts.Pull && !opts.DryRun {
		s.runPendingReverts(ctx, opts.RevertLimit, &report)
	}
	if opts.Pull {
		s.pull(ctx, opts, me, &report)
	}
	if opts.DryRun {
		if opts.Pull {
			if mirrors, err := s.store.RefreshYouTrackFields(ctx, opts.RefreshLimit); err == nil {
				report.Refreshed = len(mirrors)
			}
		}
	} else {
		s.drainOutbox(ctx, opts.OutboxLimit, &report)
		if opts.Pull {
			s.refreshCache(ctx, opts.RefreshLimit, &report)
		}
	}

	if err := s.push(ctx, opts, &report); err != nil {
		report.fail("push: %v", err)
	}
	if !opts.DryRun {
		if _, err := s.client.EnsureSavedQueries(ctx, savedQueries); err != nil {
			report.fail("saved queries: %v", err)
		}
	}
	s.syncDecisions(ctx, opts, &report)
	s.syncDocs(ctx, opts, &report)
	report.Counts = syncActionCounts(report)
	return report, nil
}

func syncActionCounts(report SyncReport) map[string]int {
	counts := map[string]int{}
	for _, action := range report.Pushed {
		if action.Adopted {
			counts["adopt"]++
		}
		switch action.Action {
		case "create":
			counts["create"]++
		case "update":
			counts["update"]++
		case "move":
			counts["move"]++
			counts["update"]++
		case "skipped":
			reason := action.Reason
			if reason == "" {
				reason = "unspecified"
			}
			counts["skip:"+reason]++
		}
	}
	for _, action := range report.Pulled {
		if action.Outcome == store.YouTrackOutcomeRejected {
			counts["refuse:"+action.Detail]++
		} else if action.Outcome == "would_skip" {
			counts["skip:"+action.Detail]++
		} else if action.Outcome == store.YouTrackOutcomeSkipped {
			counts["skip:"+action.Detail]++
		}
	}
	for _, action := range report.Outbox {
		if action.Outcome == "unmirrored" {
			counts["skip:unmirrored_outbox"]++
		}
	}
	return counts
}

// drainOutbox sends the priority and lane writes hk committed to PG. A
// failure reschedules with backoff; it never undoes the hk write.
func (s *Service) drainOutbox(ctx context.Context, limit int, report *SyncReport) {
	items, err := s.store.DueYouTrackOutbox(ctx, limit)
	if err != nil {
		report.fail("outbox: %v", err)
		return
	}
	for _, item := range items {
		action := OutboxAction{TaskID: item.TaskID, Op: item.Op}
		mirror, ok, err := s.store.GetYouTrackMirror(ctx, item.TaskID)
		if err != nil {
			report.fail("outbox task %d: %v", item.TaskID, err)
			continue
		}
		if !ok {
			_ = s.store.MarkYouTrackOutboxDone(ctx, item.ID, item.Value)
			action.Outcome = "unmirrored"
			report.Outbox = append(report.Outbox, action)
			continue
		}
		var serr error
		switch item.Op {
		case store.YouTrackOpPriority:
			n, perr := strconv.Atoi(strings.TrimSpace(item.Value))
			if perr != nil {
				serr = perr
			} else {
				serr = s.client.SetPriority(ctx, mirror.IssueID, s.schema.PriorityField, n)
			}
		case store.YouTrackOpLane:
			if e := s.ensureValue(ctx, s.schema.LaneField, item.Value, false); e != nil {
				serr = e
			} else {
				serr = s.client.SetLane(ctx, mirror.IssueID, s.schema.LaneField, item.Value)
			}
		default:
			serr = fmt.Errorf("unknown outbox op %q", item.Op)
		}
		if serr != nil {
			next := s.now().Add(outboxBackoff(item.Attempts + 1))
			if err := s.store.RescheduleYouTrackOutbox(ctx, item.ID, item.Value, item.Attempts+1, next, serr.Error()); err != nil {
				log.Printf("youtrack.outbox_reschedule_failed id=%d err=%q", item.ID, err)
			}
			action.Outcome = "retry"
			report.Outbox = append(report.Outbox, action)
			report.fail("outbox task %d %s: %v", item.TaskID, item.Op, serr)
			continue
		}
		if err := s.store.MarkYouTrackOutboxDone(ctx, item.ID, item.Value); err != nil {
			report.fail("outbox task %d: %v", item.TaskID, err)
			continue
		}
		action.Outcome = "sent"
		report.Outbox = append(report.Outbox, action)
	}
}

func outboxBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 9 {
		attempts = 9
	}
	return time.Duration(1<<attempts) * time.Second
}

func (s *Service) refreshCache(ctx context.Context, limit int, report *SyncReport) {
	mirrors, err := s.store.RefreshYouTrackFields(ctx, limit)
	if err != nil {
		report.fail("cache query: %v", err)
		return
	}
	for _, mirror := range mirrors {
		issue, err := s.client.GetIssue(ctx, mirror.IssueID)
		if err != nil {
			if merr := s.store.MarkYouTrackFieldCacheStale(ctx, mirror.TaskID); merr != nil {
				log.Printf("youtrack.cache_stale_failed task_id=%d err=%q", mirror.TaskID, merr)
			}
			report.fail("cache task %d: %v", mirror.TaskID, err)
			continue
		}
		fallback := store.Task{ID: mirror.TaskID}
		if old, ok, cerr := s.store.GetYouTrackFieldCache(ctx, mirror.TaskID); cerr != nil {
			report.fail("cache task %d: %v", mirror.TaskID, cerr)
			continue
		} else if ok {
			fallback.Priority, fallback.Lane = old.Priority, old.Lane
		} else if task, found, terr := s.store.GetTask(ctx, mirror.TaskID); terr == nil && found {
			fallback.Priority, fallback.Lane = task.Priority, task.Lane
		}
		cache := s.cacheFromIssue(mirror.TaskID, issue, fallback)
		cache.Stale = false
		if cache.Lane == "" {
			report.warn("cache task %d: no lane value to store", mirror.TaskID)
			continue
		}
		if err := s.store.PutYouTrackFieldCache(ctx, cache); err != nil {
			report.fail("cache task %d: %v", mirror.TaskID, err)
			continue
		}
		report.Refreshed++
	}
}

func (s *Service) runPendingReverts(ctx context.Context, limit int, report *SyncReport) {
	pending, err := s.store.PendingYouTrackReverts(ctx, limit)
	if err != nil {
		report.fail("pending reverts: %v", err)
		return
	}
	for _, p := range pending {
		if err := s.revert(ctx, p.IssueID, p.RevertField, p.RevertValue, p.Refusal); err != nil {
			report.fail("revert %s: %v", p.ActivityID, err)
			continue
		}
		if err := s.store.MarkYouTrackRevertDone(ctx, p.ActivityID); err != nil {
			report.fail("revert %s: %v", p.ActivityID, err)
			continue
		}
		report.Reverted++
	}
}

func (s *Service) push(ctx context.Context, opts SyncOptions, report *SyncReport) error {
	tasks, err := s.store.DueYouTrackPushTasks(ctx, opts.TaskLimit)
	if err != nil {
		return err
	}
	stalled, err := s.stalledTaskIDs(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		key := strconv.FormatInt(task.ID, 10)
		action, err := s.pushTask(ctx, task, opts, stalled[task.ID], report)
		if err != nil {
			report.fail("task %d: %v", task.ID, err)
			s.recordAttempt(ctx, opts, store.YouTrackQueueTask, key, err.Error(), "", report)
			continue
		}
		if action.Action == "skipped" {
			s.recordAttempt(ctx, opts, store.YouTrackQueueTask, key, "", store.YouTrackSkipProjectMissing, report)
		} else {
			s.recordAttempt(ctx, opts, store.YouTrackQueueTask, key, "", "", report)
		}
		report.Pushed = append(report.Pushed, action)
	}
	return nil
}

// recordAttempt stores the outcome of one queue attempt so the key waits
// its turn (failure), waits an hour (skip), or goes behind the keys that have waited
// longer (success). A dry run writes nothing, so it records nothing.
func (s *Service) recordAttempt(ctx context.Context, opts SyncOptions, queue, key, failure, skip string, report *SyncReport) {
	if opts.DryRun {
		return
	}
	if err := s.store.RecordYouTrackAttempt(ctx, queue, key, failure, skip); err != nil {
		report.fail("%s attempt %s: %v", queue, key, err)
	}
}

// stalledTaskIDs reads every task the store reports as stalled, page by page.
// The store caps one read at stalledPageSize rows; a full page means more may
// follow, so the read continues from the last id instead of treating the page
// as the whole set. A failed read fails the push: a run that could not read
// them must not clear the 멈춤 field on every issue.
func (s *Service) stalledTaskIDs(ctx context.Context) (map[int64]bool, error) {
	out := map[int64]bool{}
	after := int64(0)
	for {
		afterID := after
		tasks, err := s.store.ListTasksFiltered(ctx, store.TaskQuery{Stalled: true, AfterID: &afterID, Limit: stalledPageSize})
		if err != nil {
			return nil, fmt.Errorf("stalled tasks: %w", err)
		}
		for _, task := range tasks {
			// The cursor only moves forward. A row at or below it means the
			// read repeats a page, and continuing would never end.
			if task.ID <= after {
				return nil, fmt.Errorf("stalled tasks: task %d does not follow cursor %d", task.ID, after)
			}
			out[task.ID] = true
			after = task.ID
		}
		if len(tasks) < stalledPageSize {
			return out, nil
		}
	}
}

// stalledPageSize is one stalled-task read, the store's per-read maximum.
const stalledPageSize = 1000

// inFlight states are pushed on every pass: the PR section follows the PR's
// gate and checks, which move without a write to the task. That is the started
// category, the same set the store's scans read.
func inFlight(state string) bool {
	return store.IsStartedState(state)
}

func (s *Service) pushTask(ctx context.Context, task store.Task, opts SyncOptions, stalled bool, report *SyncReport) (PushAction, error) {
	action := PushAction{TaskID: task.ID}
	hkProject := ""
	if task.Project != nil {
		hkProject = *task.Project
	}
	project, projectID, err := s.projectForTask(ctx, hkProject, !opts.DryRun)
	if err != nil {
		if errors.Is(err, ErrProjectMissing) {
			report.warn("task %d: YouTrack 프로젝트 매핑이 없어 건너뜁니다 (%v)", task.ID, err)
			action.Action = "skipped"
			action.Reason = "project_unmapped"
			return action, nil
		}
		return action, err
	}
	mirror, mirrored, err := s.store.GetYouTrackMirror(ctx, task.ID)
	if err != nil {
		return action, err
	}
	if mirrored {
		action.IssueID, action.ReadableID = mirror.IssueID, mirror.DisplayRef
	}
	// PG columns are the execution values. The field cache is display-only;
	// a push sends what hk holds, not a possibly older YouTrack read.
	priority, lane := task.Priority, task.Lane

	needsFields := !mirrored || mirror.SyncedAt == nil || task.UpdatedAt.After(*mirror.SyncedAt) || inFlight(task.State)
	created := false
	if !mirrored {
		existing, found, err := s.client.FindIssueByField(ctx, project.ShortName, projectID, s.schema.HKIDField, task.ID)
		if err != nil {
			return action, err
		}
		if found {
			action.IssueID, action.ReadableID = existing.ID, existing.IDReadable
			action.Adopted = true
			mirror = store.YouTrackMirror{TaskID: task.ID, IssueID: existing.ID, DisplayRef: existing.IDReadable}
			if !opts.DryRun {
				// Adopt the issue without a watermark: the field push below
				// has not run yet, and MarkYouTrackTaskSynced advances it only
				// when that push succeeds.
				mirror, err = s.store.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task.ID, IssueID: existing.ID, DisplayRef: existing.IDReadable, URL: s.issueURL(existing.IDReadable), Tenant: s.tenant, Role: "mirror"})
				if err != nil {
					return action, err
				}
			}
			mirrored, needsFields = true, true
		} else {
			action.Action = "create"
			created = true
			if opts.DryRun {
				action.ReadableID = project.ShortName + "-?"
				return action, nil
			}
			view, err := s.renderTask(ctx, task)
			if err != nil {
				return action, err
			}
			fields, err := s.issueFields(ctx, task, priority, lane, view.PR, stalled)
			if err != nil {
				return action, err
			}
			summary, err := s.summary(ctx, task)
			if err != nil {
				return action, err
			}
			issue, err := s.client.CreateIssue(ctx, IssueWrite{ProjectID: projectID, Summary: cleanTitle(summary), Description: view.Text, Fields: fields})
			if err != nil {
				return action, err
			}
			action.IssueID, action.ReadableID = issue.ID, issue.IDReadable
			mirror, err = s.store.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task.ID, IssueID: issue.ID, DisplayRef: issue.IDReadable, URL: s.issueURL(issue.IDReadable), Tenant: s.tenant, Role: "mirror", SyncedAt: s.now()})
			if err != nil {
				return action, err
			}
			mirrored = true
		}
	}
	if mirrored && needsFields && !created {
		action.Action = "update"
		// A task that changed project moves with its issue; the display
		// number changes but the link stays on the stable issue id. Read the
		// current project in dry-run too, so the plan includes the move.
		current, err := s.client.GetIssue(ctx, mirror.IssueID)
		if err != nil {
			return action, err
		}
		if current.Project != nil && current.Project.ID != "" && current.Project.ID != projectID {
			action.Action = "move"
			if !opts.DryRun {
				moved, err := s.client.MoveIssue(ctx, mirror.IssueID, projectID)
				if err != nil {
					return action, err
				}
				action.ReadableID = moved.IDReadable
				if moved.IDReadable != "" {
					// Only the display number changed. The watermark advances
					// after the field push below, so a failure there keeps the
					// task due instead of stranding the unsent fields.
					if err := s.store.UpdateYouTrackMirrorDisplay(ctx, task.ID, moved.IDReadable, s.issueURL(moved.IDReadable)); err != nil {
						return action, err
					}
				}
			}
		}
		if !opts.DryRun {
			view, err := s.renderTask(ctx, task)
			if err != nil {
				return action, err
			}
			fields, err := s.issueFields(ctx, task, priority, lane, view.PR, stalled)
			if err != nil {
				return action, err
			}
			if _, err := s.client.WriteIssue(ctx, mirror.IssueID, &view.Text, fields); err != nil {
				return action, err
			}
		}
	} else if !mirrored {
		action.Action = "unchanged"
	}
	if action.Action == "" {
		action.Action = "unchanged"
	}

	comments, err := s.store.UnpushedYouTrackComments(ctx, task.ID, 0, opts.CommentLimit)
	if err != nil {
		return action, err
	}
	// marks maps hk comment ids to comments already on the issue. It is read
	// once, when the first due comment needs posting, so a comment whose create
	// response was lost is linked, not posted again.
	var marks map[int64]string
	for _, comment := range comments {
		if !humanComment(comment.Body) {
			// Machinery notes are not mirrored. Linking them with a marker that
			// no YouTrack id can equal keeps them from being due on every pass.
			if !opts.DryRun {
				if err := s.store.LinkYouTrackComment(ctx, task.ID, comment.ID, noiseLink(comment.ID), s.now()); err != nil {
					return action, err
				}
			}
			continue
		}
		if opts.DryRun {
			report.Pushed = append(report.Pushed, PushAction{TaskID: task.ID, IssueID: action.IssueID, ReadableID: action.ReadableID, Action: "comment", CommentID: comment.ID})
			continue
		}
		if action.IssueID == "" {
			return action, fmt.Errorf("no issue to comment on")
		}
		if marks == nil {
			if marks, err = s.commentMarks(ctx, action.IssueID); err != nil {
				return action, err
			}
		}
		if id, ok := marks[comment.ID]; ok {
			// Already on the issue: an earlier create was stored but its
			// response was lost. Link it and do not post it again.
			if err := s.store.LinkYouTrackComment(ctx, task.ID, comment.ID, id, s.now()); err != nil {
				return action, err
			}
			continue
		}
		mirroredComment, err := s.client.AddComment(ctx, action.IssueID, commentText(comment))
		if err != nil {
			return action, err
		}
		report.Pushed = append(report.Pushed, PushAction{TaskID: task.ID, IssueID: action.IssueID, ReadableID: action.ReadableID, Action: "comment", CommentID: comment.ID})
		if mirroredComment.ID != "" {
			if err := s.store.LinkYouTrackComment(ctx, task.ID, comment.ID, mirroredComment.ID, s.now()); err != nil {
				return action, err
			}
		}
	}
	if opts.DryRun {
		return action, nil
	}
	if mirrored {
		if err := s.store.MarkYouTrackTaskSynced(ctx, task.ID, task.UpdatedAt); err != nil {
			return action, err
		}
		if needsFields {
			// The cache is display-only. After fields are pushed it holds the
			// hk value YouTrack now carries; without a field push it is left
			// with the last read the sync made.
			cache := store.YouTrackFieldCache{TaskID: task.ID, Priority: priority, Lane: lane, Source: store.YouTrackSourceHK, FetchedAt: s.now()}
			if err := s.store.PutYouTrackFieldCache(ctx, cache); err != nil {
				return action, err
			}
		}
	}
	return action, nil
}

// permanentActivityError marks an input that can never be applied, so the
// pull consumes it instead of blocking the page forever.
func permanentActivityError(err error) bool {
	return errors.Is(err, store.ErrYouTrackActivityInput) ||
		errors.Is(err, store.ErrYouTrackTaskNotFound) ||
		errors.Is(err, store.ErrTaskNotFound) ||
		errors.Is(err, store.ErrTaskProjectRequired) ||
		strings.HasPrefix(err.Error(), "invalid task") ||
		strings.HasPrefix(err.Error(), "invalid task relane")
}

func permanentReason(err error) string {
	switch {
	case errors.Is(err, store.ErrYouTrackActivityInput):
		return "값이 hk의 형식과 맞지 않습니다"
	case errors.Is(err, store.ErrYouTrackTaskNotFound), errors.Is(err, store.ErrTaskNotFound):
		return "hk 작업을 찾지 못했습니다"
	case errors.Is(err, store.ErrTaskProjectRequired):
		return "hk 프로젝트가 설정되어 있지 않습니다"
	case strings.HasPrefix(err.Error(), "invalid task relane"):
		return "레인 이름이 올바르지 않습니다"
	}
	return "hk가 이 활동을 적용할 수 없습니다"
}

// issueFields is the custom-field write of one task. Every field the Python
// mirror wrote is written here, and the ones it cleared on a missing value
// (PR, 멈춤) are sent as null so an update clears them too.
func (s *Service) issueFields(ctx context.Context, task store.Task, priority int, lane string, pr *prView, stalled bool) ([]CustomField, error) {
	fields := []CustomField{IntField(s.schema.HKIDField, int(task.ID)), IntField(s.schema.PriorityField, priority)}
	prValue := task.Refs.PR
	if pr != nil {
		prValue = fmt.Sprintf("#%d %s", pr.Number, prStateKO(pr.State))
	}
	if prValue != "" {
		fields = append(fields, StringField(s.schema.PRField, prValue))
	} else {
		fields = append(fields, CustomField{Type: fieldSimple, Name: s.schema.PRField})
	}
	state, err := s.stateFieldValue(ctx, task.State)
	if err != nil {
		return nil, err
	}
	fields = append(fields, state)
	enums := []struct{ field, value string }{
		{s.schema.LaneField, lane},
		{s.schema.ProjectField, derefString(task.Project)},
		{s.schema.KindField, task.Kind},
	}
	grade, tier := gradeFromTitle(task.Title)
	if grade == "" && task.Refs.Linear != nil {
		grade, tier = task.Refs.Linear.Grade, task.Refs.Linear.Tier
	}
	enums = append(enums, struct{ field, value string }{s.schema.GradeField, grade}, struct{ field, value string }{s.schema.TierField, tier})
	for _, e := range enums {
		f, ok, err := s.enumFieldValue(ctx, e.field, e.value)
		if err != nil {
			return nil, err
		}
		if ok {
			fields = append(fields, f)
		}
	}
	if stalled {
		f, ok, err := s.enumFieldValue(ctx, s.schema.StallField, StallLabel)
		if err != nil {
			return nil, err
		}
		if ok {
			fields = append(fields, f)
		}
	} else {
		fields = append(fields, EmptyEnumField(s.schema.StallField))
	}
	return fields, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Service) summary(ctx context.Context, task store.Task) (string, error) {
	translations, err := s.store.ListTranslations(ctx, "task", []store.TranslationSource{{EntityID: strconv.FormatInt(task.ID, 10), Field: "title", Original: task.Title}})
	if err != nil {
		return "", err
	}
	for _, translation := range translations {
		if translation.Field == "title" && !translation.Stale && strings.TrimSpace(translation.Text) != "" {
			return translation.Text, nil
		}
	}
	return task.Title, nil
}

func (s *Service) issueURL(readable string) string {
	if readable == "" {
		return ""
	}
	return strings.TrimRight(s.client.baseURL, "/") + "/issue/" + readable
}

func (s *Service) pull(ctx context.Context, opts SyncOptions, me User, report *SyncReport) {
	cursor, err := s.store.GetYouTrackCursor(ctx)
	if err != nil {
		report.fail("cursor: %v", err)
		return
	}
	if cursor.Cursor == "" {
		s.baselinePullCursor(ctx, opts, report)
		return
	}
	page, err := s.client.ActivitiesPage(ctx, cursor.Cursor, opts.ActivityLimit)
	if err != nil {
		report.fail("activities: %v", err)
		return
	}
	applied := true
	for _, act := range page.Activities {
		in, relevant, err := s.mapActivity(ctx, act, me)
		if err != nil {
			report.fail("activity %s: %v", act.ID, err)
			applied = false
			continue
		}
		if !relevant {
			continue
		}
		if opts.DryRun {
			outcome := "would_apply"
			if in.Skip != "" {
				outcome = "would_skip"
				report.Skipped++
			}
			report.Pulled = append(report.Pulled, PullAction{ActivityID: in.ID, TaskID: in.TaskID, Outcome: outcome, Detail: in.Skip})
			continue
		}
		result, err := s.store.ApplyYouTrackActivity(ctx, in)
		if err != nil {
			if permanentActivityError(err) {
				// An input that hk can never apply must not block the page:
				// consume it as a skip and keep going.
				skip := store.YouTrackActivity{ID: in.ID, IssueID: in.IssueID, Skip: "hk가 이 활동을 적용할 수 없습니다: " + permanentReason(err)}
				if _, serr := s.store.ApplyYouTrackActivity(ctx, skip); serr != nil {
					report.fail("activity %s: %v", act.ID, serr)
					applied = false
					break
				}
				report.Skipped++
				report.Pulled = append(report.Pulled, PullAction{ActivityID: in.ID, TaskID: in.TaskID, Outcome: store.YouTrackOutcomeSkipped, Detail: "입력 오류로 건너뜀"})
				continue
			}
			report.fail("activity %s: %v", act.ID, err)
			applied = false
			break
		}
		action := PullAction{ActivityID: in.ID, TaskID: in.TaskID, Outcome: result.Outcome, Detail: result.Refusal}
		if result.Outcome == store.YouTrackOutcomeSkipped {
			report.Skipped++
			action.Detail = in.Skip
		}
		report.Pulled = append(report.Pulled, action)
		if result.Outcome == store.YouTrackOutcomeRejected && result.RevertField != "" {
			if err := s.revert(ctx, in.IssueID, result.RevertField, result.RevertValue, result.Refusal); err != nil {
				report.fail("revert %s: %v", in.ID, err)
			} else {
				if err := s.store.MarkYouTrackRevertDone(ctx, in.ID); err != nil {
					report.fail("revert %s: %v", in.ID, err)
				}
				report.Reverted++
			}
		}
	}
	if !opts.DryRun && applied && page.AfterCursor != "" {
		if err := s.store.AdvanceYouTrackCursor(ctx, page.AfterCursor); err != nil {
			report.fail("cursor advance: %v", err)
		}
	}
}

// baselinePullCursor starts opt-in pull at the current end of history. The
// API's empty cursor begins at the oldest activity, so drain pages without
// applying them before saving the final position.
func (s *Service) baselinePullCursor(ctx context.Context, opts SyncOptions, report *SyncReport) {
	cursor := ""
	for pages := 0; pages < 10000; pages++ {
		previous := cursor
		page, err := s.client.ActivitiesPage(ctx, cursor, opts.ActivityLimit)
		if err != nil {
			report.fail("activity baseline: %v", err)
			return
		}
		if page.AfterCursor != "" {
			cursor = page.AfterCursor
		}
		if !page.HasAfter {
			if cursor != "" && !opts.DryRun {
				if err := s.store.AdvanceYouTrackCursor(ctx, cursor); err != nil {
					report.fail("cursor baseline: %v", err)
				}
			}
			return
		}
		if page.AfterCursor == "" || page.AfterCursor == previous {
			report.fail("activity baseline: cursor did not advance")
			return
		}
	}
	report.fail("activity baseline: page limit exceeded")
}

// mapActivity reduces one activitiesPage row to the store input. relevant is
// false only for hk-bot's own writes; every operator activity that hk cannot
// apply becomes a skip so it is consumed once and logged.
func (s *Service) mapActivity(ctx context.Context, act Activity, me User) (store.YouTrackActivity, bool, error) {
	in := store.YouTrackActivity{ID: act.ID}
	if act.Author != nil {
		if act.Author.ID != "" && act.Author.ID == me.ID {
			return in, false, nil
		}
		if act.Author.Login != "" && (act.Author.Login == me.Login || act.Author.Login == "hk-bot") {
			return in, false, nil
		}
	}
	if act.Target != nil {
		in.IssueID = act.Target.ID
		in.ReadableID = act.Target.IDReadable
		if in.IssueID != "" {
			mirror, found, err := s.store.FindYouTrackMirrorByIssueID(ctx, in.IssueID)
			if err != nil {
				return in, true, err
			}
			if found {
				in.TaskID = mirror.TaskID
			}
		}
	}
	created := false
	unsupported := ""
	for _, category := range act.Categories {
		switch category.Type {
		case "CustomFieldCategory":
			if category.Field == nil {
				continue
			}
			name := category.Field.Name
			switch {
			case sameFieldName(name, s.schema.StateField):
				if len(category.Added) == 0 {
					unsupported = "상태 값 삭제는 hk에 적용하지 않습니다"
					continue
				}
				state, ok := StateFromValue(ActivityValueName(category.Added[0]))
				if !ok {
					unsupported = "알 수 없는 상태 값입니다"
					continue
				}
				in.State = state
			case sameFieldName(name, s.schema.PriorityField):
				if len(category.Added) == 0 {
					unsupported = "우선순위 삭제는 hk에 적용하지 않습니다"
					continue
				}
				n, ok := ActivityValueInt(category.Added[0])
				if !ok {
					unsupported = "알 수 없는 우선순위 값입니다"
					continue
				}
				in.Priority = &n
			case sameFieldName(name, s.schema.LaneField):
				if len(category.Added) == 0 {
					unsupported = "레인 삭제는 hk에 적용하지 않습니다"
					continue
				}
				in.Lane = ActivityValueName(category.Added[0])
			}
		case "CommentsCategory":
			values := category.Added
			if len(values) == 0 {
				values = act.Added
			}
			if len(values) == 0 {
				continue
			}
			text := ActivityValueText(values[0])
			commentID := ActivityValueID(values[0])
			trimmed := strings.TrimSpace(text)
			switch {
			case strings.HasPrefix(trimmed, "/지시"):
				in.Skip = "지시 명령은 아직 hk가 처리하지 않습니다"
			case strings.HasPrefix(trimmed, "/답"), strings.HasPrefix(trimmed, "/결정"):
				in.Skip = "결정 답변은 아직 hk가 처리하지 않습니다"
			case trimmed == "":
				in.Skip = "빈 코멘트입니다"
			default:
				in.Comment, in.CommentID = text, commentID
			}
		case "IssueCreatedCategory":
			created = true
		}
	}
	if created && in.TaskID == 0 {
		issue := act.Target
		if issue == nil || issue.IDReadable == "" {
			in.Skip = "새 이슈의 정보를 읽지 못했습니다"
		} else {
			newIssue, reason, err := s.newIssue(ctx, issue)
			if err != nil {
				return in, true, err
			}
			if reason != "" {
				in.Skip = reason
			} else {
				in.NewIssue = newIssue
			}
		}
	}
	if in.TaskID == 0 && in.NewIssue == nil && in.Skip == "" {
		in.Skip = unsupported
		if in.Skip == "" {
			in.Skip = "hk와 연결되지 않은 YouTrack 이슈입니다"
		}
	}
	if in.TaskID > 0 && in.State == "" && in.Priority == nil && in.Lane == "" && in.Comment == "" && in.Skip == "" {
		in.Skip = unsupported
		if in.Skip == "" {
			in.Skip = "hk가 관리하지 않는 변경입니다"
		}
	}
	return in, true, nil
}

// newIssue maps the operator's issue back to an hk project through the
// project table. An issue in an unmapped project is skipped, not applied to
// OTHER silently.
func (s *Service) newIssue(ctx context.Context, issue *Issue) (*store.YouTrackNewIssue, string, error) {
	shortName := ""
	if issue.Project != nil {
		shortName = issue.Project.ShortName
	}
	project := s.project
	if shortName != "" {
		mapped, found, err := s.store.YouTrackProjectByShortName(ctx, shortName)
		if err != nil {
			return nil, "", err
		}
		if !found {
			return nil, "매핑되지 않은 YouTrack 프로젝트의 새 이슈입니다: " + shortName, nil
		}
		if known, err := s.store.TaskProjectExists(ctx, mapped.HKProject); err != nil {
			return nil, "", err
		} else if known {
			project = mapped.HKProject
		}
	}
	lane := s.lane
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.LaneField); ok {
		if v := strings.TrimSpace(f.StringValue()); v != "" && store.ValidTaskLane(v) {
			lane = v
		}
	}
	kind := "implement"
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.KindField); ok {
		if v := strings.TrimSpace(f.StringValue()); v != "" && slices.Contains(store.TaskKinds(), v) {
			kind = v
		}
	}
	title := strings.TrimSpace(issue.Summary)
	if title == "" {
		title = issue.IDReadable
	}
	return &store.YouTrackNewIssue{IssueID: issue.ID, ReadableID: issue.IDReadable, Title: title, Lane: lane, Kind: kind, Project: project, Tenant: s.tenant}, "", nil
}

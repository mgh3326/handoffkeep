package youtrack

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// SyncOptions bound one run. The Dagu job runs every two minutes, so every
// list is a batch and one run never tries to catch up in a single pass.
type SyncOptions struct {
	DryRun        bool
	TaskLimit     int
	ActivityLimit int
	CommentLimit  int
	RefreshLimit  int
	RevertLimit   int
	OutboxLimit   int
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
	return o
}

type PushAction struct {
	TaskID     int64  `json:"task_id"`
	IssueID    string `json:"issue_id,omitempty"`
	ReadableID string `json:"readable_id,omitempty"`
	// Action is create, update, move, comment or unchanged.
	Action    string `json:"action"`
	CommentID int64  `json:"comment_id,omitempty"`
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
	DryRun    bool           `json:"dry_run"`
	Bot       string         `json:"bot,omitempty"`
	Outbox    []OutboxAction `json:"outbox"`
	Pushed    []PushAction   `json:"push"`
	Pulled    []PullAction   `json:"pull"`
	Refreshed int            `json:"refreshed"`
	Reverted  int            `json:"reverted"`
	Skipped   int            `json:"skipped"`
	Warnings  []string       `json:"warnings,omitempty"`
	Errors    []string       `json:"errors,omitempty"`
}

func (r *SyncReport) fail(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *SyncReport) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Sync runs one bounded pass: pending reverts, the priority/lane outbox, the
// field-cache refresh, the hk → YouTrack push, and one page of YouTrack → hk
// activities. In dry-run mode nothing on either side is written.
func (s *Service) Sync(ctx context.Context, opts SyncOptions) (SyncReport, error) {
	opts = opts.withDefaults()
	report := SyncReport{DryRun: opts.DryRun, Outbox: []OutboxAction{}, Pushed: []PushAction{}, Pulled: []PullAction{}}
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

	if !opts.DryRun {
		s.runPendingReverts(ctx, opts.RevertLimit, &report)
		s.drainOutbox(ctx, opts.OutboxLimit, &report)
	}
	s.refreshCache(ctx, opts.RefreshLimit, &report)

	if err := s.push(ctx, opts, &report); err != nil {
		report.fail("push: %v", err)
	}
	s.pull(ctx, opts, me, &report)
	return report, nil
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
			_ = s.store.MarkYouTrackOutboxDone(ctx, item.ID)
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
			if err := s.store.RescheduleYouTrackOutbox(ctx, item.ID, item.Attempts+1, next, serr.Error()); err != nil {
				log.Printf("youtrack.outbox_reschedule_failed id=%d err=%q", item.ID, err)
			}
			action.Outcome = "retry"
			report.Outbox = append(report.Outbox, action)
			report.fail("outbox task %d %s: %v", item.TaskID, item.Op, serr)
			continue
		}
		if err := s.store.MarkYouTrackOutboxDone(ctx, item.ID); err != nil {
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
		}
		cache := s.cacheFromIssue(mirror.TaskID, issue, fallback)
		cache.Stale = false
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
	for _, task := range tasks {
		action, err := s.pushTask(ctx, task, opts, report)
		if err != nil {
			report.fail("task %d: %v", task.ID, err)
			continue
		}
		report.Pushed = append(report.Pushed, action)
	}
	return nil
}

func (s *Service) pushTask(ctx context.Context, task store.Task, opts SyncOptions, report *SyncReport) (PushAction, error) {
	action := PushAction{TaskID: task.ID}
	hkProject := ""
	if task.Project != nil {
		hkProject = *task.Project
	}
	project, projectID, err := s.projectForTask(ctx, hkProject)
	if err != nil {
		if errors.Is(err, ErrProjectMissing) {
			report.warn("task %d: YouTrack 프로젝트 매핑이 없어 건너뜁니다 (%v)", task.ID, err)
			action.Action = "skipped"
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
	cache, cached, err := s.store.GetYouTrackFieldCache(ctx, task.ID)
	if err != nil {
		return action, err
	}
	priority, lane, source := task.Priority, task.Lane, store.YouTrackSourceHK
	if cached {
		priority, lane, source = cache.Priority, cache.Lane, cache.Source
	}

	needsFields := !mirrored || mirror.SyncedAt == nil || task.UpdatedAt.After(*mirror.SyncedAt)
	created := false
	if !mirrored {
		existing, found, err := s.client.FindIssueByHKID(ctx, s.schema.HKIDField, task.ID)
		if err != nil {
			return action, err
		}
		if found {
			action.IssueID, action.ReadableID = existing.ID, existing.IDReadable
			if !opts.DryRun {
				mirror, err = s.store.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task.ID, IssueID: existing.ID, DisplayRef: existing.IDReadable, URL: s.issueURL(existing.IDReadable), Tenant: s.tenant, Role: "mirror", SyncedAt: s.now()})
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
			fields, err := s.issueFields(ctx, task, priority, lane)
			if err != nil {
				return action, err
			}
			summary, err := s.summary(ctx, task)
			if err != nil {
				return action, err
			}
			issue, err := s.client.CreateIssue(ctx, IssueWrite{ProjectID: projectID, Summary: summary, Description: s.description(task), Fields: fields})
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
		if !opts.DryRun {
			// A task that changed project moves with its issue; the display
			// number changes but the link stays on the stable issue id.
			current, err := s.client.GetIssue(ctx, mirror.IssueID)
			if err != nil {
				return action, err
			}
			if current.Project != nil && current.Project.ID != "" && current.Project.ID != projectID {
				moved, err := s.client.MoveIssue(ctx, mirror.IssueID, projectID)
				if err != nil {
					return action, err
				}
				action.Action = "move"
				action.ReadableID = moved.IDReadable
				if moved.IDReadable != "" {
					if _, err := s.store.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task.ID, IssueID: mirror.IssueID, DisplayRef: moved.IDReadable, URL: s.issueURL(moved.IDReadable), Tenant: s.tenant, Role: mirror.Role, SyncedAt: s.now()}); err != nil {
						return action, err
					}
				}
			}
			fields, err := s.issueFields(ctx, task, priority, lane)
			if err != nil {
				return action, err
			}
			if _, err := s.client.UpdateFields(ctx, mirror.IssueID, fields); err != nil {
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
	for _, comment := range comments {
		if opts.DryRun {
			report.Pushed = append(report.Pushed, PushAction{TaskID: task.ID, IssueID: action.IssueID, ReadableID: action.ReadableID, Action: "comment", CommentID: comment.ID})
			continue
		}
		if action.IssueID == "" {
			return action, fmt.Errorf("no issue to comment on")
		}
		mirroredComment, err := s.client.AddComment(ctx, action.IssueID, comment.Body)
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
		cache = store.YouTrackFieldCache{TaskID: task.ID, Priority: priority, Lane: lane, Source: source, FetchedAt: s.now()}
		if err := s.store.PutYouTrackFieldCache(ctx, cache); err != nil {
			return action, err
		}
	}
	return action, nil
}

func (s *Service) issueFields(ctx context.Context, task store.Task, priority int, lane string) ([]CustomField, error) {
	fields := []CustomField{IntField(s.schema.HKIDField, int(task.ID)), IntField(s.schema.PriorityField, priority)}
	if task.Refs.PR != "" {
		fields = append(fields, StringField(s.schema.PRField, task.Refs.PR))
	}
	state, err := s.stateFieldValue(ctx, task.State)
	if err != nil {
		return nil, err
	}
	fields = append(fields, state)
	laneField, ok, err := s.enumFieldValue(ctx, s.schema.LaneField, lane)
	if err != nil {
		return nil, err
	}
	if ok {
		fields = append(fields, laneField)
	}
	if task.Kind != "" {
		f, ok, err := s.enumFieldValue(ctx, s.schema.KindField, task.Kind)
		if err != nil {
			return nil, err
		}
		if ok {
			fields = append(fields, f)
		}
	}
	if task.Refs.Linear != nil {
		for _, pair := range [][2]string{{s.schema.GradeField, task.Refs.Linear.Grade}, {s.schema.TierField, task.Refs.Linear.Tier}} {
			f, ok, err := s.enumFieldValue(ctx, pair[0], pair[1])
			if err != nil {
				return nil, err
			}
			if ok {
				fields = append(fields, f)
			}
		}
	}
	return fields, nil
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

func (s *Service) description(task store.Task) string {
	return fmt.Sprintf("%s/ui/tasks/%d\n\n%s", strings.TrimRight(s.console, "/"), task.ID, task.Title)
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
	if applied && page.AfterCursor != "" {
		if err := s.store.AdvanceYouTrackCursor(ctx, page.AfterCursor); err != nil {
			report.fail("cursor advance: %v", err)
		}
	}
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
		if v := strings.TrimSpace(f.StringValue()); v != "" {
			lane = v
		}
	}
	kind := "implement"
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.KindField); ok {
		if v := strings.TrimSpace(f.StringValue()); v != "" {
			kind = v
		}
	}
	title := strings.TrimSpace(issue.Summary)
	if title == "" {
		title = issue.IDReadable
	}
	return &store.YouTrackNewIssue{IssueID: issue.ID, ReadableID: issue.IDReadable, Title: title, Lane: lane, Kind: kind, Project: project, Tenant: s.tenant}, "", nil
}

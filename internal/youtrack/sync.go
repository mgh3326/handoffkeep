package youtrack

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

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
	return o
}

type PushAction struct {
	TaskID     int64  `json:"task_id"`
	IssueID    string `json:"issue_id,omitempty"`
	ReadableID string `json:"readable_id,omitempty"`
	// Action is create, update, comment or unchanged.
	Action    string `json:"action"`
	CommentID int64  `json:"comment_id,omitempty"`
}

type PullAction struct {
	ActivityID string `json:"activity_id"`
	TaskID     int64  `json:"task_id,omitempty"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail,omitempty"`
}

type SyncReport struct {
	DryRun    bool         `json:"dry_run"`
	Bot       string       `json:"bot,omitempty"`
	Pushed    []PushAction `json:"push"`
	Pulled    []PullAction `json:"pull"`
	Refreshed int          `json:"refreshed"`
	Reverted  int          `json:"reverted"`
	Skipped   int          `json:"skipped"`
	Errors    []string     `json:"errors,omitempty"`
}

func (r *SyncReport) fail(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

// Sync runs one bounded pass: pending reverts, the field-cache refresh, the
// hk → YouTrack push, and one page of YouTrack → hk activities. In dry-run
// mode nothing on either side is written.
func (s *Service) Sync(ctx context.Context, opts SyncOptions) (SyncReport, error) {
	opts = opts.withDefaults()
	report := SyncReport{DryRun: opts.DryRun, Pushed: []PushAction{}, Pulled: []PullAction{}}
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
	}
	s.refreshCache(ctx, opts.RefreshLimit, &report)

	if err := s.push(ctx, opts, &report); err != nil {
		report.fail("push: %v", err)
	}
	s.pull(ctx, opts, me, &report)
	return report, nil
}

func (s *Service) refreshCache(ctx context.Context, limit int, report *SyncReport) {
	links, err := s.store.RefreshYouTrackFields(ctx, s.ttl, limit)
	if err != nil {
		report.fail("cache query: %v", err)
		return
	}
	for _, link := range links {
		issue, err := s.client.GetIssue(ctx, link.IssueID)
		if err != nil {
			if merr := s.store.MarkYouTrackFieldCacheStale(ctx, link.TaskID); merr != nil {
				log.Printf("youtrack.cache_stale_failed task_id=%d err=%q", link.TaskID, merr)
			}
			report.fail("cache task %d: %v", link.TaskID, err)
			continue
		}
		fallback := store.Task{ID: link.TaskID}
		if old, ok, cerr := s.store.GetYouTrackFieldCache(ctx, link.TaskID); cerr != nil {
			report.fail("cache task %d: %v", link.TaskID, cerr)
			continue
		} else if ok {
			fallback.Priority, fallback.Lane = old.Priority, old.Lane
		}
		cache := s.cacheFromIssue(link.TaskID, issue, fallback)
		cache.Stale = false
		if err := s.store.PutYouTrackFieldCache(ctx, cache); err != nil {
			report.fail("cache task %d: %v", link.TaskID, err)
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
	link, linked, err := s.store.GetYouTrackLink(ctx, task.ID)
	if err != nil {
		return action, err
	}
	if linked {
		action.IssueID, action.ReadableID = link.IssueID, link.ReadableID
	}
	cache, cached, err := s.store.GetYouTrackFieldCache(ctx, task.ID)
	if err != nil {
		return action, err
	}
	priority, lane := task.Priority, task.Lane
	if cached {
		priority, lane = cache.Priority, cache.Lane
	}

	needsFields := !linked || link.PushedTaskUpdatedAt == nil || task.UpdatedAt.After(*link.PushedTaskUpdatedAt)
	created := false
	if !linked {
		existing, found, err := s.client.FindIssueByHKID(ctx, s.schema.Project, s.schema.HKIDField, task.ID)
		if err != nil {
			return action, err
		}
		if found {
			action.IssueID, action.ReadableID = existing.ID, existing.IDReadable
			if !opts.DryRun {
				link, err = s.store.LinkYouTrackIssue(ctx, task.ID, existing.ID, existing.IDReadable, s.now())
				if err != nil {
					return action, err
				}
			}
			linked, needsFields = true, true
		} else {
			action.Action = "create"
			created = true
			if opts.DryRun {
				action.ReadableID = s.schema.Project + "-?"
				return action, nil
			}
			fields, err := s.issueFields(ctx, task, priority, lane)
			if err != nil {
				return action, err
			}
			projectID, err := s.projectID(ctx)
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
			link, err = s.store.LinkYouTrackIssue(ctx, task.ID, issue.ID, issue.IDReadable, s.now())
			if err != nil {
				return action, err
			}
			linked = true
		}
	}
	if linked && needsFields && !created {
		action.Action = "update"
		if !opts.DryRun {
			fields, err := s.issueFields(ctx, task, priority, lane)
			if err != nil {
				return action, err
			}
			if _, err := s.client.UpdateFields(ctx, link.IssueID, fields); err != nil {
				return action, err
			}
		}
	} else if !linked {
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
		mirrored, err := s.client.AddComment(ctx, action.IssueID, comment.Body)
		if err != nil {
			return action, err
		}
		report.Pushed = append(report.Pushed, PushAction{TaskID: task.ID, IssueID: action.IssueID, ReadableID: action.ReadableID, Action: "comment", CommentID: comment.ID})
		if mirrored.ID != "" {
			if err := s.store.LinkYouTrackComment(ctx, task.ID, comment.ID, mirrored.ID, s.now()); err != nil {
				return action, err
			}
		}
	}
	if opts.DryRun {
		return action, nil
	}
	if linked {
		if err := s.store.MarkYouTrackTaskPushed(ctx, task.ID, task.UpdatedAt); err != nil {
			return action, err
		}
		if err := s.store.SetYouTrackCachedPriority(ctx, task.ID, priority, s.now()); err != nil {
			return action, err
		}
		if err := s.store.SetYouTrackCachedLane(ctx, task.ID, lane, s.now()); err != nil {
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
	if task.Project != nil && *task.Project != "" {
		f, ok, err := s.enumFieldValue(ctx, s.schema.ProjectField, *task.Project)
		if err != nil {
			return nil, err
		}
		if ok {
			fields = append(fields, f)
		}
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
		if in.ReadableID != "" {
			link, found, err := s.store.FindYouTrackLinkByReadableID(ctx, in.ReadableID)
			if err != nil {
				return in, true, err
			}
			if found {
				in.TaskID = link.TaskID
				if in.IssueID == "" {
					in.IssueID = link.IssueID
				}
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
			newIssue, err := s.newIssue(ctx, issue)
			if err != nil {
				return in, true, err
			}
			in.NewIssue = newIssue
			in.TaskID = 0
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

func (s *Service) newIssue(ctx context.Context, issue *Issue) (*store.YouTrackNewIssue, error) {
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
	project := s.project
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.ProjectField); ok {
		if v := strings.TrimSpace(f.StringValue()); v != "" {
			known, err := s.store.TaskProjectExists(ctx, v)
			if err != nil {
				return nil, err
			}
			if known {
				project = v
			}
		}
	}
	title := strings.TrimSpace(issue.Summary)
	if title == "" {
		title = issue.IDReadable
	}
	return &store.YouTrackNewIssue{IssueID: issue.ID, ReadableID: issue.IDReadable, Title: title, Lane: lane, Kind: kind, Project: project}, nil
}

func (s *Service) revert(ctx context.Context, issueID, field, value, refusal string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("no issue id for the revert")
	}
	switch field {
	case "state":
		state := strings.TrimSpace(value)
		if state == "" {
			return nil
		}
		label := StateLabel(state)
		if err := s.ensureValue(ctx, s.schema.StateField, label, StateResolved(state)); err != nil {
			return err
		}
		if err := s.client.SetStateValue(ctx, issueID, s.schema.StateField, label, StateResolved(state)); err != nil {
			return err
		}
	case "priority":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("revert priority %q: %w", value, err)
		}
		if err := s.client.SetPriority(ctx, issueID, s.schema.PriorityField, n); err != nil {
			return err
		}
	case "lane":
		lane := strings.TrimSpace(value)
		if lane == "" {
			return nil
		}
		if err := s.ensureValue(ctx, s.schema.LaneField, lane, false); err != nil {
			return err
		}
		if err := s.client.SetLane(ctx, issueID, s.schema.LaneField, lane); err != nil {
			return err
		}
	}
	reason := strings.TrimSpace(refusal)
	if reason == "" {
		reason = "hk가 이 변경을 적용하지 않았습니다"
	}
	_, err := s.client.AddComment(ctx, issueID, "hk가 거부: "+reason)
	return err
}

package youtrack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// decisionListLimit bounds one status read, as the Python mirror did.
const decisionListLimit = 200

// decisionStatuses are the statuses the DEC project mirrors.
var decisionStatuses = []string{
	store.DecisionStatusOpen, store.DecisionStatusAnswered, store.DecisionStatusSuperseded, store.DecisionStatusExpired,
}

// DecisionAction is one DEC issue a run wrote, or would write in dry-run.
type DecisionAction struct {
	DecisionID int64  `json:"decision_id"`
	TaskID     *int64 `json:"task_id,omitempty"`
	IssueID    string `json:"issue_id,omitempty"`
	ReadableID string `json:"readable_id,omitempty"`
	// Action is create, update or unchanged.
	Action string `json:"action"`
}

// decisionStateLabel is the 결정 상태 value of a request.
func decisionStateLabel(d store.DecisionRecord) string {
	switch d.Status {
	case store.DecisionStatusOpen:
		if d.Overdue {
			return "기한 경과"
		}
		return "대기"
	case store.DecisionStatusAnswered:
		return "답변됨"
	case store.DecisionStatusSuperseded:
		return "대체됨"
	case store.DecisionStatusExpired:
		return "만료"
	}
	return "대기"
}

// decisionView is the question and the choices a DEC issue shows. Both are
// the 3514 display rendering of the request when the task carries one, and
// the stored text otherwise. The stored question and options are never changed.
type decisionView struct {
	Question    string
	Options     []store.DecisionOption
	AllowFree   bool
	TaskIssue   string
	Recommended string
}

// decisionViewOf reads the task that asked the question (for its display
// template and its YouTrack issue) and picks the text to show.
func (s *Service) decisionViewOf(ctx context.Context, d store.DecisionRecord) (decisionView, error) {
	v := decisionView{Question: d.Question, Recommended: d.Recommended}
	var options *store.DecisionOptions = d.Options
	if d.TaskID != nil {
		task, found, err := s.store.GetTask(ctx, *d.TaskID)
		if err != nil {
			return decisionView{}, fmt.Errorf("decision task %d: %w", *d.TaskID, err)
		}
		if found && task.Refs.DecisionRequest != nil && task.Refs.DecisionRequest.ID == d.SourceRef && task.Refs.DecisionRequest.Question == d.Question {
			if rendered, ok := store.RenderDecisionTemplate(*task.Refs.DecisionRequest, task.Refs.DecisionOptions); ok {
				// The choices go in their own section, so the question text
				// carries the heading and the fields only.
				v.Question = store.DecisionTemplateText(store.DecisionTemplate{Question: rendered.Question})
				options = &rendered.Options
			}
		}
		mirror, ok, err := s.store.GetYouTrackMirror(ctx, *d.TaskID)
		if err != nil {
			return decisionView{}, err
		}
		if ok {
			v.TaskIssue = mirror.DisplayRef
		}
	}
	if options != nil {
		v.Options = options.Options
		v.AllowFree = options.AllowFree
	}
	return v, nil
}

// decisionBody is the DEC issue text, in the order the Python mirror wrote it.
func decisionBody(console string, d store.DecisionRecord, v decisionView) string {
	console = strings.TrimRight(console, "/")
	lines := []string{fmt.Sprintf("**답하기:** [hk 콘솔 결정 %d](%s/ui/decisions/%d)", d.ID, console, d.ID)}
	task := fmt.Sprintf("레인 `%s`", d.Lane)
	if d.TaskID != nil {
		task += fmt.Sprintf(" · 작업 [hk %d](%s/ui/tasks/%d)", *d.TaskID, console, *d.TaskID)
		if v.TaskIssue != "" {
			task += " · YouTrack " + v.TaskIssue
		}
	}
	lines = append(lines, task, "", "## 질문", v.Question, "", "## 선택지")
	for _, o := range v.Options {
		item := fmt.Sprintf("- **%s** %s", o.Key, o.Label)
		if o.Recommended || o.Key == d.Recommended {
			item += " ✓ 권장"
		}
		lines = append(lines, item)
	}
	if v.AllowFree {
		lines = append(lines, "- 직접 답 가능")
	}
	defaultAction := d.DefaultAction
	if defaultAction == "" {
		defaultAction = "없음"
	}
	due := "없음"
	if d.DueAt != nil {
		due = d.DueAt.UTC().Format(descriptionTimeLayout)
	}
	lines = append(lines, "", "답이 없으면: "+defaultAction, "기한: "+due)
	if d.Answer != "" {
		answeredAt := ""
		if d.AnsweredAt != nil {
			answeredAt = d.AnsweredAt.UTC().Format(descriptionTimeLayout)
		}
		lines = append(lines, "", fmt.Sprintf("## 답 (%s, %s)", d.AnsweredBy, answeredAt), d.Answer)
	}
	lines = append(lines, "", "_hk에서 읽기 전용으로 옮긴 내용입니다._")
	return strings.Join(lines, "\n")
}

// decisionSummary is the DEC issue title: the request id and the first line
// of the question.
func decisionSummary(d store.DecisionRecord, question string) string {
	first := ""
	if lines := strings.Split(strings.TrimSpace(question), "\n"); len(lines) > 0 {
		first = strings.TrimSpace(lines[0])
	}
	if first == "" {
		first = "(질문 없음)"
	}
	return truncateRunes(fmt.Sprintf("[결정 %d] %s", d.ID, first), summaryLimit)
}

// decisionFields are the custom fields of one DEC issue.
func (s *Service) decisionFields(ctx context.Context, d store.DecisionRecord) ([]CustomField, error) {
	var taskID any
	if d.TaskID != nil {
		taskID = *d.TaskID
	}
	fields := []CustomField{
		StateField(DecStateField, decisionStateLabel(d), false),
		IntField(DecIDField, int(d.ID)),
		{Type: fieldSimple, Name: s.schema.HKIDField, Value: taskID},
		DateField(DecDueField, dueMillis(d.DueAt)),
	}
	if d.Recommended != "" {
		fields = append(fields, StringField(DecRecommendedField, d.Recommended))
	} else {
		fields = append(fields, CustomField{Type: fieldSimple, Name: DecRecommendedField})
	}
	if d.Lane != "" {
		f, ok, err := s.enumFieldValue(ctx, s.schema.LaneField, d.Lane)
		if err != nil {
			return nil, err
		}
		if ok {
			fields = append(fields, f)
		}
	}
	return fields, nil
}

func dueMillis(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

// syncDecisions mirrors the hk decision requests into the DEC project. A
// request is written when its updated_at has moved since the last write. A
// closed request is mirrored only when it already has an issue, so the
// project does not fill with old history.
func (s *Service) syncDecisions(ctx context.Context, opts SyncOptions, report *SyncReport) {
	projectID, err := s.client.ProjectID(ctx, DecisionProject)
	if err != nil {
		report.fail("decision project %s: %v", DecisionProject, err)
		return
	}
	now := s.now()
	var rows []store.DecisionRecord
	for _, status := range decisionStatuses {
		list, err := s.store.ListDecisionRecords(ctx, store.DecisionRecordFilter{Status: status, IncludeSnoozed: true, Limit: decisionListLimit}, now)
		if err != nil {
			report.fail("decisions %s: %v", status, err)
			return
		}
		rows = append(rows, list...)
	}
	ids := make([]int64, 0, len(rows))
	for _, d := range rows {
		ids = append(ids, d.ID)
	}
	mirrors, err := s.store.ListYouTrackDecisionMirrors(ctx, ids)
	if err != nil {
		report.fail("decision mirrors: %v", err)
		return
	}
	for _, d := range rows {
		if err := s.syncDecision(ctx, d, projectID, mirrors, opts, report); err != nil {
			report.fail("decision %d: %v", d.ID, err)
		}
	}
}

func (s *Service) syncDecision(ctx context.Context, d store.DecisionRecord, projectID string, mirrors map[int64]store.YouTrackDecisionMirror, opts SyncOptions, report *SyncReport) error {
	stamp := d.UpdatedAt.UTC().Format(time.RFC3339Nano)
	mirror, mirrored := mirrors[d.ID]
	if mirrored && mirror.Stamp == stamp {
		return nil
	}
	if !mirrored {
		if d.Status != store.DecisionStatusOpen {
			return nil
		}
		// A request with no row may already have an issue, from the Python
		// mirror or from an earlier run that did not record it. Find it by its
		// 결정 ID before a create.
		found, ok, err := s.client.FindIssueByField(ctx, DecisionProject, projectID, DecIDField, d.ID)
		if err != nil {
			return err
		}
		if ok {
			mirror = store.YouTrackDecisionMirror{DecisionID: d.ID, IssueID: found.ID, DisplayRef: found.IDReadable}
			mirrored = true
		}
	}
	view, err := s.decisionViewOf(ctx, d)
	if err != nil {
		return err
	}
	action := DecisionAction{DecisionID: d.ID, TaskID: d.TaskID}
	if mirrored {
		action.IssueID, action.ReadableID, action.Action = mirror.IssueID, mirror.DisplayRef, "update"
	} else {
		action.Action = "create"
	}
	if opts.DryRun {
		report.Decisions = append(report.Decisions, action)
		return nil
	}
	fields, err := s.decisionFields(ctx, d)
	if err != nil {
		return err
	}
	body := decisionBody(s.console, d, view)
	if mirrored {
		issue, err := s.client.WriteIssue(ctx, mirror.IssueID, &body, fields)
		if err != nil {
			return err
		}
		mirror.DisplayRef = issue.IDReadable
	} else {
		issue, err := s.client.CreateIssue(ctx, IssueWrite{ProjectID: projectID, Summary: decisionSummary(d, view.Question), Description: body, Fields: fields})
		if err != nil {
			return err
		}
		mirror = store.YouTrackDecisionMirror{DecisionID: d.ID, IssueID: issue.ID, DisplayRef: issue.IDReadable}
	}
	mirror.Stamp = stamp
	if err := s.store.PutYouTrackDecisionMirror(ctx, mirror); err != nil {
		return err
	}
	action.IssueID, action.ReadableID = mirror.IssueID, mirror.DisplayRef
	report.Decisions = append(report.Decisions, action)
	return nil
}

package ui

import (
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Decision requests (#618) are read-only here. One record — the task's
// refs.decision_request plus its task_events snapshots — feeds the queue row
// badge (from the list refs), the drawer card (boardTaskDetail) and the
// Decisions section; the display state is derived in one place,
// store.DecisionRequestState. Answering stays with #580.

// decisionStateLabels are the operator-facing names of the derived states.
// "overdue" deliberately says the default was NOT applied: a passed deadline
// is never shown as an application.
var decisionStateLabels = map[string]string{
	store.DecisionViewOpen:           "열림 · 응답 대기",
	store.DecisionViewOverdue:        "기한 경과 · 기본값 미적용",
	store.DecisionViewUncleaned:      "미정리 요청 · 종료된 태스크에 열려 있음",
	store.DecisionViewAnswered:       "답변됨",
	store.DecisionViewDefaultApplied: "기본값 적용됨",
	store.DecisionViewWithdrawn:      "철회됨",
	store.DecisionViewSuperseded:     "대체됨",
	decisionViewUnrecorded:           "미기록",
}

// decisionViewUnrecorded marks a needs_decision task with no open structured
// request: the question exists only as a transition note.
const decisionViewUnrecorded = "unrecorded"

type decisionRequestView struct {
	ID             string                    `json:"id"`
	Revision       int                       `json:"revision"`
	Supersedes     string                    `json:"supersedes,omitempty"`
	SupersededBy   string                    `json:"superseded_by,omitempty"`
	Current        bool                      `json:"current"`
	State          string                    `json:"state"`
	StateLabel     string                    `json:"state_label"`
	Question       string                    `json:"question"`
	Options        []store.DecisionOption    `json:"options"`
	AllowFree      bool                      `json:"allow_free"`
	Reason         string                    `json:"reason,omitempty"`
	DefaultAction  string                    `json:"default_action"`
	DefaultOption  string                    `json:"default_option,omitempty"`
	DefaultTrigger string                    `json:"default_trigger,omitempty"`
	DueAt          *time.Time                `json:"due_at,omitempty"`
	Doc            string                    `json:"doc,omitempty"`
	RequestedBy    string                    `json:"requested_by"`
	RequestedAt    time.Time                 `json:"requested_at"`
	Resolution     *store.DecisionResolution `json:"resolution,omitempty"`
}

// legacyDecisionView is a needs_decision task whose question was never
// recorded as a request. It is shown as 미기록 — never as an open request
// with a recommendation or default it does not have.
type legacyDecisionView struct {
	State      string                 `json:"state"`
	StateLabel string                 `json:"state_label"`
	Question   string                 `json:"question"`
	Options    []store.DecisionOption `json:"options"`
	AllowFree  bool                   `json:"allow_free"`
}

func projectDecisionRequest(entry store.DecisionRequestEntry, taskState string, now time.Time) decisionRequestView {
	request := entry.Request
	state := store.DecisionRequestState(request, taskState, entry.SupersededBy != "", now)
	view := decisionRequestView{
		ID: request.ID, Revision: request.Revision, Supersedes: request.Supersedes, SupersededBy: strings.TrimPrefix(entry.SupersededBy, "?"),
		Current: entry.Current, State: state, StateLabel: decisionStateLabels[state],
		Question: request.Question, Options: []store.DecisionOption{}, Reason: request.Reason,
		DefaultAction: request.DefaultAction, DefaultOption: request.DefaultOption, DefaultTrigger: request.DefaultTrigger,
		DueAt: request.DueAt, Doc: request.Doc, RequestedBy: request.RequestedBy, RequestedAt: request.RequestedAt.UTC(),
		Resolution: request.Resolution,
	}
	if entry.Options != nil {
		view.Options = append(view.Options, entry.Options.Options...)
		view.AllowFree = entry.Options.AllowFree
	}
	return view
}

// taskDecisionViews returns every request of a task (newest first) and, for
// a needs_decision task without an open request, the unrecorded question.
func taskDecisionViews(task store.Task, now time.Time) ([]decisionRequestView, *legacyDecisionView) {
	entries := store.DecisionRequestHistory(task)
	views := make([]decisionRequestView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, projectDecisionRequest(entry, task.State, now))
	}
	if task.State != "needs_decision" || task.Refs.Disposition != nil || (task.Refs.DecisionRequest != nil && task.Refs.DecisionRequest.Status == store.DecisionRequestOpen) {
		return views, nil
	}
	legacy := &legacyDecisionView{State: decisionViewUnrecorded, StateLabel: decisionStateLabels[decisionViewUnrecorded], Options: []store.DecisionOption{}}
	for _, event := range task.Events {
		if event.Kind == store.TaskEventTransition && event.To == "needs_decision" {
			legacy.Question = event.Note
		}
	}
	// A request's choices belong to that request; only a task that never had
	// one shows its transition-era options here.
	if task.Refs.DecisionRequest == nil && task.Refs.DecisionOptions != nil {
		legacy.Options = append(legacy.Options, task.Refs.DecisionOptions.Options...)
		legacy.AllowFree = task.Refs.DecisionOptions.AllowFree
	}
	return views, legacy
}

type decisionRequestCard struct {
	Task store.Task
	View decisionRequestView
}

// decisionRequestSection is the Decisions page block. Pending and Uncleaned
// come from store.DecisionRequestCounts — the same split the queue shows.
type decisionRequestSection struct {
	Pending   int
	Uncleaned int
	Truncated bool
	Items     []decisionRequestCard
}

const decisionRequestListLimit = 1000

func (h *Handler) decisionRequestData(r *http.Request) (decisionRequestSection, error) {
	tasks, err := h.store.ListOpenDecisionRequests(r.Context(), decisionRequestListLimit)
	if err != nil {
		return decisionRequestSection{}, err
	}
	now := time.Now().UTC()
	section := decisionRequestSection{Truncated: len(tasks) == decisionRequestListLimit}
	section.Pending, section.Uncleaned = store.DecisionRequestCounts(tasks)
	for _, task := range tasks {
		entry := store.DecisionRequestEntry{Request: *task.Refs.DecisionRequest, Options: task.Refs.DecisionOptions, Current: true}
		section.Items = append(section.Items, decisionRequestCard{Task: task, View: projectDecisionRequest(entry, task.State, now)})
	}
	return section, nil
}

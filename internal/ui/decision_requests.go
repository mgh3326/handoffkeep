package ui

import (
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

// decisionOptionView is one option as the console renders it: the stored
// option plus, for each of its pictures and videos, the MIME type and
// original name the page needs to show it in place. The stored record keeps
// only the address and the caption; the media fields are read from the
// attachments table for this response.
type decisionOptionView struct {
	Key         string                   `json:"key"`
	Label       string                   `json:"label"`
	Recommended bool                     `json:"recommended,omitempty"`
	Attachments []decisionAttachmentView `json:"attachments,omitempty"`
}

// decisionAttachmentView is one attachment of an option. MIME, OriginalName
// and SizeBytes are empty when the attachments table no longer holds the
// address: the page then shows a plain link, never a broken picture.
// SizeBytes also lets the page keep a file too large to render in place a
// link, the same threshold the task page uses.
type decisionAttachmentView struct {
	SHA256       string `json:"sha256"`
	Caption      string `json:"caption,omitempty"`
	MIME         string `json:"mime,omitempty"`
	OriginalName string `json:"original_name,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
}

func projectDecisionOptions(options []store.DecisionOption) []decisionOptionView {
	out := make([]decisionOptionView, 0, len(options))
	for _, option := range options {
		view := decisionOptionView{Key: option.Key, Label: option.Label, Recommended: option.Recommended}
		for _, attachment := range option.Attachments {
			view.Attachments = append(view.Attachments, decisionAttachmentView{SHA256: attachment.SHA256, Caption: attachment.Caption})
		}
		out = append(out, view)
	}
	return out
}

// storeOptions is the stored shape of these options, for the translation
// source hash: media metadata is read from the attachments table and is not
// part of what a translator translated.
func storeOptions(options []decisionOptionView) []store.DecisionOption {
	out := make([]store.DecisionOption, 0, len(options))
	for _, option := range options {
		stored := store.DecisionOption{Key: option.Key, Label: option.Label, Recommended: option.Recommended}
		for _, attachment := range option.Attachments {
			stored.Attachments = append(stored.Attachments, store.DecisionOptionAttachment{SHA256: attachment.SHA256, Caption: attachment.Caption})
		}
		out = append(out, stored)
	}
	return out
}

type decisionRequestView struct {
	ID              string               `json:"id"`
	Revision        int                  `json:"revision"`
	Supersedes      string               `json:"supersedes,omitempty"`
	SupersededBy    string               `json:"superseded_by,omitempty"`
	Current         bool                 `json:"current"`
	State           string               `json:"state"`
	StateLabel      string               `json:"state_label"`
	Question        string               `json:"question"`
	QuestionKO      string               `json:"question_ko,omitempty"`
	QuestionKOStale bool                 `json:"question_ko_stale,omitempty"`
	Options         []decisionOptionView `json:"options"`
	OptionsKO       []decisionOptionView `json:"options_ko,omitempty"`
	OptionsKOStale  bool                 `json:"options_ko_stale,omitempty"`
	AllowFree       bool                 `json:"allow_free"`
	Reason          string               `json:"reason,omitempty"`
	// RecommendationReason, Unverified and OneFix are the requester's longer
	// account of its recommendation, what it could not check, and its one
	// suggested fix (hk 4788).
	RecommendationReason string                    `json:"recommendation_reason,omitempty"`
	Unverified           []string                  `json:"unverified,omitempty"`
	OneFix               string                    `json:"one_fix,omitempty"`
	DefaultAction        string                    `json:"default_action"`
	DefaultOption        string                    `json:"default_option,omitempty"`
	DefaultTrigger       string                    `json:"default_trigger,omitempty"`
	DueAt                *time.Time                `json:"due_at,omitempty"`
	Doc                  string                    `json:"doc,omitempty"`
	Refs                 []string                  `json:"refs,omitempty"`
	RequestedBy          string                    `json:"requested_by"`
	RequestedAt          time.Time                 `json:"requested_at"`
	Resolution           *store.DecisionResolution `json:"resolution,omitempty"`
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
	switch request.Status {
	case store.DecisionRequestOpen, store.DecisionRequestAnswered, store.DecisionRequestDefaultApplied, store.DecisionRequestWithdrawn:
	default:
		state = request.Status
	}
	if request.Status == store.DecisionRequestDefaultApplied && (request.Resolution == nil || request.Resolution.Receipt == "") {
		state = "default_applied_without_receipt"
	}
	view := decisionRequestView{
		ID: request.ID, Revision: request.Revision, Supersedes: request.Supersedes, SupersededBy: strings.TrimPrefix(entry.SupersededBy, "?"),
		Current: entry.Current, State: state, StateLabel: decisionStateLabels[state],
		Question: request.Question, Options: []decisionOptionView{}, Reason: request.Reason,
		RecommendationReason: request.RecommendationReason, Unverified: request.Unverified, OneFix: request.OneFix,
		DefaultAction: request.DefaultAction, DefaultOption: request.DefaultOption, DefaultTrigger: request.DefaultTrigger,
		DueAt: request.DueAt, Doc: request.Doc, Refs: request.Refs, RequestedBy: request.RequestedBy, RequestedAt: request.RequestedAt.UTC(),
		Resolution: request.Resolution,
	}
	if entry.Options != nil {
		view.Options = projectDecisionOptions(entry.Options.Options)
		view.AllowFree = entry.Options.AllowFree
	}
	if request.Status == store.DecisionRequestOpen {
		if rendered, ok := store.RenderDecisionTemplate(request, entry.Options); ok {
			view.QuestionKO = rendered.Question
			view.OptionsKO = projectDecisionOptions(rendered.Options.Options)
			view.Options = projectDecisionOptions(rendered.Options.Options)
			for i := range view.Options {
				view.Options[i].Label = entry.Options.Options[i].Label
			}
			view.Reason = rendered.Reason
		}
	}
	return view
}

// decisionMediaSHAs lists the content addresses the views name, for one
// lookup of the MIME type and original name each picture or video is shown
// with. The translated options name the same media, so they are read too: an
// option on the wire carries one attachment shape whichever group it is in.
func decisionMediaSHAs(views []decisionRequestView) []string {
	shas := []string{}
	seen := map[string]bool{}
	for _, view := range views {
		for _, options := range [][]decisionOptionView{view.Options, view.OptionsKO} {
			for _, option := range options {
				for _, attachment := range option.Attachments {
					if !seen[attachment.SHA256] {
						seen[attachment.SHA256] = true
						shas = append(shas, attachment.SHA256)
					}
				}
			}
		}
	}
	return shas
}

// taskDecisionViews returns every request of a task (newest first) and, for
// a needs_decision task without an open request, the unrecorded question.
func taskDecisionViews(task store.Task, now time.Time) ([]decisionRequestView, *legacyDecisionView) {
	entries := store.DecisionRequestHistory(task)
	views := make([]decisionRequestView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, projectDecisionRequest(entry, task.State, now))
	}
	if !task.AwaitingDecision || task.Refs.Disposition != nil || (task.Refs.DecisionRequest != nil && task.Refs.DecisionRequest.Status == store.DecisionRequestOpen) {
		return views, nil
	}
	legacy := &legacyDecisionView{State: decisionViewUnrecorded, StateLabel: decisionStateLabels[decisionViewUnrecorded], Options: []store.DecisionOption{}}
	for _, event := range task.Events {
		if event.SetsAwaiting() {
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

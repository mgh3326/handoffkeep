package ui

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// attentionDeployItems is how many pending deploys the Home list carries; the
// count covers all of them.
const attentionDeployItems = 5

type attentionDecisionView struct {
	Key             string     `json:"key"`
	Source          string     `json:"source"`
	ID              int64      `json:"id"`
	Lane            string     `json:"lane"`
	Question        string     `json:"question"`
	QuestionKO      string     `json:"question_ko,omitempty"`
	QuestionKOStale bool       `json:"question_ko_stale,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	DueAt           *time.Time `json:"due_at"`
	Overdue         bool       `json:"overdue"`
	Read            bool       `json:"read"`
	Woken           bool       `json:"woken"`
}

type attentionTaskView struct {
	Key          string    `json:"key"`
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	TitleKO      string    `json:"title_ko,omitempty"`
	TitleKOStale bool      `json:"title_ko_stale,omitempty"`
	Lane         string    `json:"lane"`
	State        string    `json:"state"`
	Priority     int       `json:"priority"`
	Reason       string    `json:"reason"`
	Since        time.Time `json:"since"`
	Read         bool      `json:"read"`
	Woken        bool      `json:"woken"`
}

type attentionDeployView struct {
	Key          string    `json:"key"`
	Service      string    `json:"service"`
	TaskID       int64     `json:"task_id"`
	Title        string    `json:"title"`
	TitleKO      string    `json:"title_ko,omitempty"`
	TitleKOStale bool      `json:"title_ko_stale,omitempty"`
	PR           string    `json:"pr"`
	MergedAt     time.Time `json:"merged_at"`
	Read         bool      `json:"read"`
	Woken        bool      `json:"woken"`
}

type attentionResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	Decisions   struct {
		Count   int                     `json:"count"`
		Overdue int                     `json:"overdue"`
		Items   []attentionDecisionView `json:"items"`
	} `json:"decisions"`
	Tasks struct {
		Count            int                 `json:"count"`
		AwaitingDecision int                 `json:"awaiting_decision"`
		Stalled          int                 `json:"stalled"`
		Items            []attentionTaskView `json:"items"`
	} `json:"tasks"`
	Deploys struct {
		Count        int                      `json:"count"`
		EventsCapped bool                     `json:"events_capped"`
		Items        []attentionDeployView    `json:"items"`
		Lagging      []attentionLaggingDeploy `json:"lagging,omitempty"`
	} `json:"deploys"`
}

type attentionLaggingDeploy struct {
	Service  string `json:"service"`
	Repo     string `json:"repo"`
	LagCount int    `json:"lag_count"`
}

// deployInboxKey is the attention item key of one deploy row. It carries the
// service so one task merged into two services stays two rows.
func deployInboxKey(service string, taskID int64) string {
	return "deploy:" + service + ":" + strconv.FormatInt(taskID, 10)
}

// attention serves GET /ui/api/attention: what is waiting on the operator, as
// one bounded document built from the database alone. It never reaches the
// hub, so a hub outage cannot slow or fail it. Items the operator completed or
// snoozed are excluded here and in the counts; a snooze that has come due is
// shown marked woken.
func (h *Handler) attention(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	// Task titles and deploy state are operator reading material; a service
	// principal has no page that reads this.
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	now := time.Now().UTC()
	waiting, err := h.store.Attention(r.Context(), now)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}
	if h.ytTasks != nil {
		// The task half reads YouTrack (hk 4430); decisions and deploys stay
		// hk's. A YouTrack failure fails the page rather than showing hk's
		// task copy.
		snap, err := h.ytTasks.snapshot(r.Context())
		if err != nil {
			youTrackReadFailed(w, r, err)
			return
		}
		candidates, err := h.store.AttentionTaskCandidates(r.Context(), now, snap.startedIDs())
		if err != nil {
			h.attentionFailed(w, err)
			return
		}
		waiting.Tasks = youTrackAttentionTasks(candidates, snap)
	}
	deploys, err := h.pendingDeploys(r)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}
	deployKeys := make([]string, 0, len(deploys.Services))
	for _, service := range deploys.Services {
		for _, task := range service.MergedSince {
			deployKeys = append(deployKeys, deployInboxKey(service.Service, task.TaskID))
		}
	}
	// Deploy rows come from documents, not the attention snapshot, so their
	// triage state is a second read. A row written between the two reads can
	// make a count and its list disagree; both reads are marked stale by the
	// next poll.
	states, err := h.store.InboxStates(r.Context(), deployKeys)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}

	response := attentionResponse{GeneratedAt: now}
	response.Decisions.Count = waiting.Decisions.Count
	response.Decisions.Overdue = waiting.Decisions.Overdue
	response.Decisions.Items = make([]attentionDecisionView, 0, len(waiting.Decisions.Items))
	for _, x := range waiting.Decisions.Items {
		response.Decisions.Items = append(response.Decisions.Items, attentionDecisionView{
			Key: x.Key, Source: x.Source, ID: x.ID, Lane: x.Lane, Question: x.Question,
			CreatedAt: x.CreatedAt, DueAt: x.DueAt, Overdue: x.Overdue, Read: x.Read, Woken: x.Woken,
		})
	}
	response.Tasks.Count = waiting.Tasks.Count
	response.Tasks.AwaitingDecision = waiting.Tasks.AwaitingDecision
	response.Tasks.Stalled = waiting.Tasks.Stalled
	response.Tasks.Items = make([]attentionTaskView, 0, len(waiting.Tasks.Items))
	for _, x := range waiting.Tasks.Items {
		response.Tasks.Items = append(response.Tasks.Items, attentionTaskView{
			Key: x.Key, ID: x.ID, Title: x.Title, Lane: x.Lane, State: x.State, Priority: x.Priority,
			Reason: x.Reason, Since: x.Since, Read: x.Read, Woken: x.Woken,
		})
	}
	response.Deploys.EventsCapped = deploys.EventsCapped
	response.Deploys.Count, response.Deploys.Items = attentionDeploys(deploys, states, now)
	for _, svc := range deploys.Services {
		if svc.Lagging {
			response.Deploys.Lagging = append(response.Deploys.Lagging, attentionLaggingDeploy{
				Service:  svc.Service,
				Repo:     svc.Repo,
				LagCount: svc.LagCount,
			})
		}
	}
	taskSources := make([]store.TranslationSource, 0, len(response.Tasks.Items)+len(response.Deploys.Items))
	for _, x := range response.Tasks.Items {
		taskSources = append(taskSources, store.TranslationSource{EntityID: strconv.FormatInt(x.ID, 10), Field: "title", Original: x.Title})
	}
	for _, x := range response.Deploys.Items {
		taskSources = append(taskSources, store.TranslationSource{EntityID: strconv.FormatInt(x.TaskID, 10), Field: "title", Original: x.Title})
	}
	taskTranslations, err := h.store.ListTranslations(r.Context(), "task", taskSources)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}
	for i := range response.Tasks.Items {
		for _, tr := range taskTranslations {
			if tr.EntityID == strconv.FormatInt(response.Tasks.Items[i].ID, 10) {
				response.Tasks.Items[i].TitleKO = tr.Text
				response.Tasks.Items[i].TitleKOStale = tr.Stale
			}
		}
	}
	for i := range response.Deploys.Items {
		for _, tr := range taskTranslations {
			if tr.EntityID == strconv.FormatInt(response.Deploys.Items[i].TaskID, 10) {
				response.Deploys.Items[i].TitleKO = tr.Text
				response.Deploys.Items[i].TitleKOStale = tr.Stale
			}
		}
	}
	decisionSources := make([]store.TranslationSource, 0, len(waiting.Decisions.Items))
	for _, x := range waiting.Decisions.Items {
		if strings.HasPrefix(x.SourceRef, "dr-") {
			decisionSources = append(decisionSources, store.TranslationSource{EntityID: x.SourceRef, Field: "question", Original: x.Question})
		}
	}
	decisionTranslations, err := h.store.ListTranslations(r.Context(), "decision_request", decisionSources)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}
	for i, x := range waiting.Decisions.Items {
		for _, tr := range decisionTranslations {
			if tr.EntityID == x.SourceRef {
				response.Decisions.Items[i].QuestionKO = tr.Text
				response.Decisions.Items[i].QuestionKOStale = tr.Stale
			}
		}
	}
	writeBoardJSON(w, response)
}

func (h *Handler) attentionFailed(w http.ResponseWriter, err error) {
	log.Printf("ui.attention_error err=%q", err)
	writeJSONError(w, http.StatusInternalServerError, "internal_error")
}

// attentionDeploys counts every task merged since its service's last deploy
// and returns the newest attentionDeployItems of them. Rows the operator
// completed or snoozed are left out of both.
func attentionDeploys(pending deployPendingResponse, states map[string]store.InboxState, now time.Time) (int, []attentionDeployView) {
	all := []attentionDeployView{}
	for _, service := range pending.Services {
		for _, task := range service.MergedSince {
			key := deployInboxKey(service.Service, task.TaskID)
			state := states[key]
			// A done covers the merge it was given for: a later merge into
			// the same service brings the row back, as it does for a task.
			if state.DoneAt != nil && !state.DoneAt.Before(task.MergedAt) {
				continue
			}
			if state.SnoozedUntil != nil && state.SnoozedUntil.After(now) {
				continue
			}
			all = append(all, attentionDeployView{
				Key: key, Service: service.Service, TaskID: task.TaskID, Title: task.Title, PR: task.PR, MergedAt: task.MergedAt,
				Read: state.ReadAt != nil, Woken: state.Woken(now),
			})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].MergedAt.Equal(all[j].MergedAt) {
			return all[i].MergedAt.After(all[j].MergedAt)
		}
		return all[i].TaskID > all[j].TaskID
	})
	count := len(all)
	if count > attentionDeployItems {
		all = all[:attentionDeployItems]
	}
	return count, all
}

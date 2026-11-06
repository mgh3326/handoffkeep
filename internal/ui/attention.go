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
	Source          string     `json:"source"`
	ID              int64      `json:"id"`
	Lane            string     `json:"lane"`
	Question        string     `json:"question"`
	QuestionKO      string     `json:"question_ko,omitempty"`
	QuestionKOStale bool       `json:"question_ko_stale,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	DueAt           *time.Time `json:"due_at"`
	Overdue         bool       `json:"overdue"`
}

type attentionTaskView struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	TitleKO      string    `json:"title_ko,omitempty"`
	TitleKOStale bool      `json:"title_ko_stale,omitempty"`
	Lane         string    `json:"lane"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	Since        time.Time `json:"since"`
}

type attentionDeployView struct {
	Service      string    `json:"service"`
	TaskID       int64     `json:"task_id"`
	Title        string    `json:"title"`
	TitleKO      string    `json:"title_ko,omitempty"`
	TitleKOStale bool      `json:"title_ko_stale,omitempty"`
	PR           string    `json:"pr"`
	MergedAt     time.Time `json:"merged_at"`
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
		Count        int                   `json:"count"`
		EventsCapped bool                  `json:"events_capped"`
		Items        []attentionDeployView `json:"items"`
	} `json:"deploys"`
}

// attention serves GET /ui/api/attention: what is waiting on the operator, as
// one bounded document built from the database alone. It never reaches the
// hub, so a hub outage cannot slow or fail it.
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
	deploys, err := h.pendingDeploys(r)
	if err != nil {
		h.attentionFailed(w, err)
		return
	}

	response := attentionResponse{GeneratedAt: now}
	response.Decisions.Count = waiting.Decisions.Count
	response.Decisions.Overdue = waiting.Decisions.Overdue
	response.Decisions.Items = make([]attentionDecisionView, 0, len(waiting.Decisions.Items))
	for _, x := range waiting.Decisions.Items {
		response.Decisions.Items = append(response.Decisions.Items, attentionDecisionView{Source: x.Source, ID: x.ID, Lane: x.Lane, Question: x.Question, CreatedAt: x.CreatedAt, DueAt: x.DueAt, Overdue: x.Overdue})
	}
	response.Tasks.Count = waiting.Tasks.Count
	response.Tasks.AwaitingDecision = waiting.Tasks.AwaitingDecision
	response.Tasks.Stalled = waiting.Tasks.Stalled
	response.Tasks.Items = make([]attentionTaskView, 0, len(waiting.Tasks.Items))
	for _, x := range waiting.Tasks.Items {
		response.Tasks.Items = append(response.Tasks.Items, attentionTaskView{ID: x.ID, Title: x.Title, Lane: x.Lane, State: x.State, Reason: x.Reason, Since: x.Since})
	}
	response.Deploys.EventsCapped = deploys.EventsCapped
	response.Deploys.Count, response.Deploys.Items = attentionDeploys(deploys)
	taskSources := make([]store.TranslationSource, 0, len(waiting.Tasks.Items)+len(response.Deploys.Items))
	for _, x := range waiting.Tasks.Items {
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
// and returns the newest attentionDeployItems of them.
func attentionDeploys(pending deployPendingResponse) (int, []attentionDeployView) {
	all := []attentionDeployView{}
	for _, service := range pending.Services {
		for _, task := range service.MergedSince {
			all = append(all, attentionDeployView{Service: service.Service, TaskID: task.TaskID, Title: task.Title, PR: task.PR, MergedAt: task.MergedAt})
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

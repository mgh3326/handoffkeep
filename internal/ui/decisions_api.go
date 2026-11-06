package ui

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func (h *Handler) decisionsAPI(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	now := time.Now().UTC()
	if r.URL.Path == "/ui/api/decisions" {
		tab := r.URL.Query().Get("tab")
		if tab == "" {
			tab = "open"
		}
		result, err := h.store.ListDecisionTab(r.Context(), tab, now, 200)
		if errors.Is(err, store.ErrInvalidDecisionTab) {
			writeJSONError(w, http.StatusBadRequest, "invalid_tab")
			return
		}
		if err != nil {
			decisionsFailed(w, err)
			return
		}
		writeBoardJSON(w, struct {
			GeneratedAt time.Time `json:"generated_at"`
			Tab         string    `json:"tab"`
			Counts      struct {
				Open    int `json:"open"`
				Overdue int `json:"overdue"`
			} `json:"counts"`
			Truncated bool                    `json:"truncated"`
			Items     []store.DecisionTabItem `json:"items"`
		}{GeneratedAt: now, Tab: tab, Counts: struct {
			Open    int `json:"open"`
			Overdue int `json:"overdue"`
		}{result.Open, result.Overdue}, Truncated: result.Truncated, Items: result.Items})
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/ui/api/decisions/")
	if !taskIDPattern.MatchString(raw) {
		writeJSONError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	id, _ := strconv.ParseInt(raw, 10, 64)
	record, found, err := h.store.GetDecisionRecord(r.Context(), id, now)
	if err != nil {
		decisionsFailed(w, err)
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "not_found")
		return
	}
	var task *struct {
		ID      int64   `json:"id"`
		Title   string  `json:"title"`
		State   string  `json:"state"`
		Lane    string  `json:"lane"`
		Project *string `json:"project"`
	}
	type requestView struct {
		decisionRequestView
		HumanOnly    bool   `json:"human_only"`
		AnswerPolicy string `json:"answer_policy"`
	}
	var request *requestView
	history := []decisionRequestView{}
	attempts := []store.DecisionAttempt{}
	reason := "source_elsewhere"
	if record.TaskID != nil {
		t, ok, e := h.store.GetTask(r.Context(), *record.TaskID)
		if e != nil {
			decisionsFailed(w, e)
			return
		}
		if ok {
			task = &struct {
				ID      int64   `json:"id"`
				Title   string  `json:"title"`
				State   string  `json:"state"`
				Lane    string  `json:"lane"`
				Project *string `json:"project"`
			}{t.ID, t.Title, t.State, t.Lane, t.Project}
			entries := store.DecisionRequestHistory(t)
			for _, entry := range entries {
				v := projectDecisionRequest(entry, t.State, now)
				history = append(history, v)
				if record.Source == store.DecisionSourceTaskRequest && entry.Request.ID == record.SourceRef {
					policy := entry.Request.EffectivePolicy(t.Refs.Disposition != nil)
					request = &requestView{v, entry.Request.HumanOnly, policy}
					reason = ""
					if !entry.Current || (record.Status == store.DecisionStatusSuperseded && record.SupersededBy != "withdrawn") {
						reason = "superseded"
					} else if entry.Request.Status != store.DecisionRequestOpen || record.Status != store.DecisionStatusOpen {
						reason = "closed"
					}
				}
			}
			attempts, e = h.store.ListDecisionAttempts(r.Context(), t.ID, 1000)
			if e != nil {
				decisionsFailed(w, e)
				return
			}
		}
	}
	if record.Source == store.DecisionSourceTaskRequest && request == nil {
		reason = "superseded"
	}
	replacement, err := h.store.DecisionReplacement(r.Context(), record.SupersededBy)
	if err != nil {
		decisionsFailed(w, err)
		return
	}
	type recordView struct {
		store.DecisionRecord
		SupersededRecordID *int64 `json:"superseded_record_id"`
	}
	writeBoardJSON(w, struct {
		GeneratedAt    time.Time               `json:"generated_at"`
		Record         recordView              `json:"record"`
		Task           any                     `json:"task"`
		Request        any                     `json:"request"`
		History        []decisionRequestView   `json:"history"`
		Attempts       []store.DecisionAttempt `json:"attempts"`
		Answerable     bool                    `json:"answerable"`
		DisabledReason string                  `json:"disabled_reason"`
	}{now, recordView{record, replacement}, task, request, history, attempts, reason == "", reason})
}

func decisionsFailed(w http.ResponseWriter, err error) {
	log.Printf("ui.decisions_error err=%q", err)
	writeJSONError(w, http.StatusInternalServerError, "internal_error")
}

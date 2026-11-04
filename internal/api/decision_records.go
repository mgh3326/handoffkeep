package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// decisionRecordsList serves GET /v1/decision-requests: the decision records
// of every source, oldest first. status defaults to open; a snoozed open row
// is listed only with include_snoozed=true.
func (s Server) decisionRecordsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	limit, err := queryLimit(r, 1000, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	includeSnoozed := false
	if v := q.Get("include_snoozed"); v != "" {
		if includeSnoozed, err = strconv.ParseBool(v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	now := time.Now().UTC()
	xs, err := s.Service.Store.ListDecisionRecords(r.Context(), store.DecisionRecordFilter{
		Status: q.Get("status"), Lane: q.Get("lane"), Source: q.Get("source"), IncludeSnoozed: includeSnoozed, Limit: limit,
	}, now)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"server_time": now, "decision_requests": xs})
}

type decisionSnoozeInput struct {
	Until time.Time `json:"until"`
}

// decisionRecordSnooze serves POST /v1/decision-requests/{id}/snooze. It
// writes the record only, never the question's source.
func (s Server) decisionRecordSnooze(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input decisionSnoozeInput
	if err := decode(r, &input, 1024); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.SnoozeDecisionRecord(r.Context(), id, input.Until, time.Now().UTC())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// decisionSources serves GET /v1/decision-requests/sources: every question
// each source counts as open, with the task and job it names. It only reads;
// handoffkeep decisions backfill --dry-run classifies it.
func (s Server) decisionSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	out, err := s.Service.Store.ListOpenDecisionSources(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

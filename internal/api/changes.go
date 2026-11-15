package api

import (
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// changesList is GET /v1/changes?task=N or ?pr=PR_URL: the work record's
// changes with their linked tasks and review passes. Exactly one selector.
func (s Server) changesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	if q.Has("task") == q.Has("pr") {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	// after_id keeps the id order and starts after that id; absent answers
	// exactly as before.
	var afterID *int64
	if q.Has("after_id") {
		id, parseErr := queryCursor(r, "after_id")
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		afterID = &id
	}
	var xs []store.Change
	if q.Has("task") {
		taskID, parseErr := strconv.ParseInt(q.Get("task"), 10, 64)
		if parseErr != nil || taskID < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		xs, err = s.Service.Store.ListChangesByTask(r.Context(), taskID, afterID, limit)
	} else {
		xs, err = s.Service.Store.ListChangesByPR(r.Context(), q.Get("pr"), afterID, limit)
	}
	if err != nil {
		appErr(w, r, err)
		return
	}
	var last int64
	if len(xs) > 0 {
		last = xs[len(xs)-1].ID
	}
	jsonOut(w, http.StatusOK, listBody("changes", xs, afterID, nextAfterID(afterID, len(xs), limit, last)))
}

// changeMergeFlow is POST /v1/changes/{id}/merge-flow. It is an operator
// control, not a tool record: it replaces the change's priority, hold_reason
// and dependency, and only the operator clients may call it.
func (s Server) changeMergeFlow(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID && client != operatorDeskClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var input store.ChangeMergeFlow
	if err = decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	c, err := s.Service.Store.SetChangeMergeFlow(r.Context(), id, input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, c)
}

// changeVerification is POST /v1/changes/verifications: the verdict for one
// head of a change (hk 3120). Any client may write; recorded_by is the
// client. 201 on create, 200 on a replay or a replaced verifier-blocked row.
func (s Server) changeVerification(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input store.VerificationInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	v, created, err := s.Service.Store.RecordVerification(r.Context(), input, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, v)
}

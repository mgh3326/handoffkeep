package api

import (
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// dotsResultsList is GET /v1/dots/results?task=N or ?issue=<identifier>.
func (s Server) dotsResultsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	n, err := queryLimit(r, 100, store.DotsResultListMax)
	if err != nil || (q.Get("task") == "") == (q.Get("issue") == "") {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var taskID int64
	if q.Get("task") != "" {
		if taskID, err = strconv.ParseInt(q.Get("task"), 10, 64); err != nil || taskID < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	xs, err := s.Service.dotsResults().ListDotsResults(r.Context(), taskID, q.Get("issue"), n)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"results": xs})
}

// dotsAdoption is POST /v1/dots/results/{id}/adoption. A change and a
// repeat of the stored change both answer 200; changed tells them apart.
func (s Server) dotsAdoption(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var in store.DotsAdoptionInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	res, changed, err := s.Service.dotsResults().SetDotsAdoption(r.Context(), id, client, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"result": res, "changed": changed})
}

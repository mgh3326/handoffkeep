package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// commandsList pages operator commands for one executor lane. The executor
// polls its own lane; state narrows the window. Ordering is id order (the
// created_at order), so after_id never skips a row.
func (s Server) commandsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	afterID, err := queryCursor(r, "after_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	lane := r.URL.Query().Get("executor_lane")
	if lane == "" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.Store.ListOperatorCommands(r.Context(), lane, r.URL.Query().Get("state"), afterID, limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"commands": xs})
}

func (s Server) commandReceipt(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var in store.OperatorCommandReceiptInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.RecordOperatorCommandReceipt(r.Context(), id, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

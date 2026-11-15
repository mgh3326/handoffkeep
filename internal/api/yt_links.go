package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ytLinksStatus is GET /v1/yt/links/status: the issue-link projection's state
// for the operator (hk 4640, `youtrack links status`). Query: task (one task's
// rows and held ops; an unknown task is 404 not_found), limit (rows per list).
// It is read-only, so any bearer client may read it.
func (s Server) ytLinksStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 100, store.YTOpListMax)
	if err != nil {
		appErr(w, r, err)
		return
	}
	var task int64
	if r.URL.Query().Has("task") {
		if task, err = queryCursor(r, "task"); err != nil || task < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	status, err := s.Service.Store.YTLinksStatusOf(r.Context(), task, limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, status)
}

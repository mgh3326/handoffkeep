package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ytOps is GET /v1/yt/ops: the YouTrack write ops (slice b1a) for the
// operator to watch. Query: state (a lifecycle state, "pending" — the default:
// prepared, dispatched and remote_unknown — or "all"), task, after_id, limit.
// Each op is a ytwrite.OpView: no body, no raw error, no key; remote_unknown
// reads as pending, never done. next_after_id is set when the page is full.
func (s Server) ytOps(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	limit, err := queryLimit(r, 100, store.YTOpListMax)
	if err != nil {
		appErr(w, r, err)
		return
	}
	var task, after int64
	if q.Has("task") {
		if task, err = queryCursor(r, "task"); err != nil || task < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	if q.Has("after_id") {
		if after, err = queryCursor(r, "after_id"); err != nil {
			appErr(w, r, err)
			return
		}
	}
	ops, err := s.Service.Store.ListYTWriteOps(r.Context(), store.YTOpFilter{State: q.Get("state"), TaskID: task, AfterID: after, Limit: limit})
	if err != nil {
		appErr(w, r, err)
		return
	}
	views := make([]ytwrite.OpView, 0, len(ops))
	for _, op := range ops {
		views = append(views, ytwrite.ViewOf(op))
	}
	var next any
	if len(ops) == limit {
		next = ops[len(ops)-1].ID
	}
	jsonOut(w, http.StatusOK, map[string]any{"ops": views, "next_after_id": next})
}

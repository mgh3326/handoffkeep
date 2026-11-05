package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ApplyTaskEvent applies one integration event to a task (S2).
func (s Service) ApplyTaskEvent(ctx context.Context, in store.TaskSourceEvent) (store.TaskEventResult, error) {
	return s.Store.ApplyTaskSourceEvent(ctx, in)
}

// ListTasksFiltered lists tasks with the full filter set, stalled included.
func (s Service) ListTasksFiltered(ctx context.Context, q store.TaskQuery) ([]store.Task, error) {
	return s.Store.ListTasksFiltered(ctx, q)
}

// PreviewBacklogSweep returns what the backlog sweep would park for days and
// writes nothing.
func (s Service) PreviewBacklogSweep(ctx context.Context, days int) ([]store.SweepCandidate, error) {
	return s.Store.SweepBacklog(ctx, days, true)
}

// taskEvents is POST /v1/task-events: a mergewake event for one task. The
// answer says whether it applied and the task's state; a redelivered event
// changes nothing.
func (s Server) taskEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input store.TaskSourceEvent
	if err := decode(r, &input, bodyEvent); err != nil {
		appErr(w, r, err)
		return
	}
	res, err := s.Service.ApplyTaskEvent(r.Context(), input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, res)
}

// tasksSweepPreview is GET /v1/tasks/sweep?days=N: the backlog tasks the
// sweep would park at that setting. It writes nothing; the sweep itself runs
// in serve when backlog_icebox_days is set.
func (s Server) tasksSweepPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.auth(w, r); !ok {
		return
	}
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days < 1 || store.CheckBacklogIceboxDays(days) != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.PreviewBacklogSweep(r.Context(), days)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"days": days, "dry_run": true, "tasks": xs})
}

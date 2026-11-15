package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

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

// TaskDrift reads the drift report as of now and writes nothing.
func (s Service) TaskDrift(ctx context.Context, mergedDays int) (store.TaskDrift, error) {
	return s.Store.TaskDrift(ctx, time.Now().UTC(), mergedDays)
}

// SettleMerged applies the merged-task rule (store.SettleMerged). A dry run
// writes nothing.
func (s Service) SettleMerged(ctx context.Context, project string, dryRun bool) (store.SettleMergedReport, error) {
	return s.Store.SettleMerged(ctx, project, dryRun)
}

func (s Service) TaskHygiene(ctx context.Context, now time.Time, project string, dryRun bool) (store.TaskHygieneReport, error) {
	return s.Store.TaskHygiene(ctx, now, project, dryRun)
}

// tasksSettleMerged is POST /v1/tasks/settle-merged: body {"project": "",
// "dry_run": true}. dry_run defaults to true when omitted, so a bare POST
// writes nothing; the CLI always sends it explicitly.
func (s Server) tasksSettleMerged(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	input := struct {
		Project string `json:"project"`
		DryRun  *bool  `json:"dry_run"`
	}{}
	if err := decode(r, &input, bodyTiny); err != nil {
		appErr(w, r, err)
		return
	}
	dryRun := input.DryRun == nil || *input.DryRun
	rep, err := s.Service.SettleMerged(r.Context(), input.Project, dryRun)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, rep)
}

// tasksHygiene is POST /v1/tasks/hygiene: body {"project": "", "dry_run": true}.
// dry_run defaults to true when omitted, so a bare POST writes nothing; the
// CLI always sends it explicitly.
func (s Server) tasksHygiene(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	input := struct {
		Project string `json:"project"`
		DryRun  *bool  `json:"dry_run"`
	}{}
	if err := decode(r, &input, bodyTiny); err != nil {
		appErr(w, r, err)
		return
	}
	dryRun := input.DryRun == nil || *input.DryRun
	rep, err := s.Service.TaskHygiene(r.Context(), time.Now().UTC(), input.Project, dryRun)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, rep)
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

// tasksDrift is GET /v1/tasks/drift?merged_days=N: the drift report, the tasks
// whose state disagrees with the records around them. It writes nothing; the
// operator or the desk decides what to move. merged_days defaults to 2.
func (s Server) tasksDrift(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.auth(w, r); !ok {
		return
	}
	mergedDays := store.DriftDefaultMergedDays
	if raw := r.URL.Query().Get("merged_days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || store.CheckDriftMergedDays(days) != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		mergedDays = days
	}
	rep, err := s.Service.TaskDrift(r.Context(), mergedDays)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, rep)
}

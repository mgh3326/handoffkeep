package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// tasksSearchMaxLimit is the widest window GET /v1/tasks/search reads: the
// store's scope-tasks cap, so a filtered request still sees every candidate
// the text search ranked.
const tasksSearchMaxLimit = 50

// SearchTasks finds tasks by words across every state. Words match the same
// rows the task search matches: titles (plus Korean title translations),
// translated bodies and comment bodies. Results come back in search order —
// relevance first, newest first within equal rank — with truncated reporting
// a cut window. state accepts one current or old state name; openOnly keeps
// only unfinished tasks (everything but merged, deployed, completed, dropped
// and duplicate) and is exclusive with state; a nil project means no project
// filter while "" selects legacy rows with no project.
func (s Service) SearchTasks(ctx context.Context, q, lane, state string, project *string, openOnly bool, limit int) ([]store.Task, bool, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > tasksSearchMaxLimit {
		limit = tasksSearchMaxLimit
	}
	var filter store.StateFilter
	filtered := openOnly
	if state != "" {
		// The handler names the parameter; this stays a plain code for
		// direct callers only.
		f, ok := store.ParseStateFilter(state)
		if !ok {
			return nil, false, errors.New("invalid task search state")
		}
		filter = f
		filtered = true
	}
	if project != nil && *project != "" {
		filtered = true
	}
	searchLimit := limit
	if filtered {
		// Filters apply after the text search, so a filtered request reads
		// the whole window before it limits; otherwise hits that do not
		// match a filter hide matching hits ranked just after them.
		searchLimit = tasksSearchMaxLimit
	}
	results, err := s.Search(ctx, q, "tasks", lane, searchLimit)
	if err != nil {
		return nil, false, err
	}
	ids := make([]int64, 0, len(results))
	truncated := false
	for _, r := range results {
		if r.Truncated {
			truncated = true
		}
		id, err := strconv.ParseInt(r.Key, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	// One batch in search order: a GetTask per hit costs about six
	// statements each.
	byID := map[int64]store.Task{}
	if len(ids) > 0 {
		xs, err := s.Store.TasksWithSpawns(ctx, ids)
		if err != nil {
			return nil, false, err
		}
		for _, x := range xs {
			byID[x.ID] = x
		}
	}
	out := make([]store.Task, 0, len(ids))
	for _, id := range ids {
		x, found := byID[id]
		if !found {
			continue
		}
		if state != "" && !searchStateMatches(x, filter) {
			continue
		}
		if openOnly && store.IsSettledState(x.State) {
			continue
		}
		if project != nil {
			if *project == "" {
				if x.Project != nil {
					continue
				}
			} else if x.Project == nil || *x.Project != *project {
				continue
			}
		}
		out = append(out, x)
	}
	// A cut search window stays truncated even when the filtered page is
	// full: matches may exist beyond the window.
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// searchStateMatches reports whether x passes a parsed state filter: the
// awaiting_decision flag for the old needs_decision name, else membership.
func searchStateMatches(x store.Task, filter store.StateFilter) bool {
	if filter.Awaiting {
		return x.AwaitingDecision
	}
	for _, state := range filter.States {
		if x.State == state {
			return true
		}
	}
	return false
}

// tasksSearch serves GET /v1/tasks/search: one command's duplicate check
// across every state (hk 6479). It reuses the existing task search and its
// bearer authentication; it adds no credential class.
func (s Server) tasksSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeParamError(w, http.StatusBadRequest, "invalid_context", "q", "non-empty text")
		return
	}
	limit, ok := taskLimit(w, r, 20, tasksSearchMaxLimit, "invalid_context")
	if !ok {
		return
	}
	lane := strings.TrimSpace(r.URL.Query().Get("lane"))
	if lane != "" && !store.ValidProjectName(lane) {
		writeParamError(w, http.StatusBadRequest, "invalid_context", "lane", paramAllowedNames)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state != "" {
		if _, ok := store.ParseStateFilter(state); !ok {
			writeParamError(w, http.StatusBadRequest, "invalid_context", "state", store.AcceptedStateNames())
			return
		}
	}
	var project *string
	if r.URL.Query().Has("project") {
		v := strings.TrimSpace(r.URL.Query().Get("project"))
		project = &v
		if v != "" && !store.ValidProjectName(v) {
			writeParamError(w, http.StatusBadRequest, "invalid_context", "project", paramAllowedNames)
			return
		}
	}
	var openOnly bool
	if r.URL.Query().Has("open") {
		v, err := strconv.ParseBool(r.URL.Query().Get("open"))
		if err != nil {
			writeParamError(w, http.StatusBadRequest, "invalid_context", "open", "true,false")
			return
		}
		openOnly = v
	}
	if openOnly && state != "" {
		writeParamError(w, http.StatusBadRequest, "invalid_context", "open", "open cannot be combined with state")
		return
	}
	xs, truncated, err := s.Service.SearchTasks(r.Context(), q, lane, state, project, openOnly, limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"tasks": xs, "truncated": truncated})
}

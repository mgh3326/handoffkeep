package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// importExport is the read-only, cursor-paged source for YouTrack custom
// imports. The dedicated grant cannot authorize any other /v1 operation.
func (s Server) importExport(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if !s.ImportReadGrants[client] {
		writeError(w, http.StatusForbidden, "import_read_forbidden")
		return
	}
	q := r.URL.Query()
	allowed := map[string]bool{"limit": true, "after": true, "after_at": true, "after_id": true, "project": true}
	for key := range q {
		if !allowed[key] || len(q[key]) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		appErr(w, r, err)
		return
	}
	resource := r.PathValue("resource")
	ctx := r.Context()
	switch resource {
	case "projects":
		if q.Has("after_at") || q.Has("after_id") || q.Has("project") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		projects, err := s.Service.Store.ListTaskProjects(ctx)
		if err != nil {
			appErr(w, r, err)
			return
		}
		sort.Strings(projects)
		after := q.Get("after")
		filtered := make([]string, 0, len(projects))
		for _, p := range projects {
			if p > after {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) > limit {
			next := filtered[limit-1]
			jsonOut(w, http.StatusOK, map[string]any{"items": filtered[:limit], "next_cursor": next})
			return
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": filtered, "next_cursor": nil})
	case "tasks":
		if q.Has("after") || q.Has("after_at") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 999 {
			limit = 999
		}
		after := int64(0)
		if q.Has("after_id") {
			after, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		var project *string
		if q.Has("project") {
			p := q.Get("project")
			if p == "" {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			if p == "/unassigned" {
				p = ""
			}
			project = &p
		}
		rows, err := s.Service.Store.ListTasksPage(ctx, "", "", "", project, nil, after, limit+1)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "comments":
		if q.Has("after") || q.Has("after_at") || q.Has("project") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit >= store.TaskCommentListMax {
			limit = store.TaskCommentListMax - 1
		}
		after := int64(0)
		if q.Has("after_id") {
			after, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		rows, err := s.Service.Store.ImportCommentsPage(ctx, after, limit)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) == limit {
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "documents":
		if q.Has("after_at") || q.Has("after") || !q.Has("project") || q.Get("project") == "" {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 20 {
			limit = 20
		}
		after := int64(0)
		if q.Has("after_id") {
			after, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		rows, err := s.Service.Store.ImportDocumentsPage(ctx, q.Get("project"), after, limit+1)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "document-authors":
		if q.Has("after_at") || q.Has("after") || !q.Has("project") || q.Get("project") == "" {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 20 {
			limit = 20
		}
		after := int64(0)
		if q.Has("after_id") {
			after, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		rows, err := s.Service.Store.ImportDocumentAuthorsPage(ctx, q.Get("project"), after, limit+1)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "decisions":
		if q.Has("after") || q.Has("project") || q.Has("after_at") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 999 {
			limit = 999
		}
		afterID := int64(0)
		if q.Has("after_id") {
			afterID, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		rows, err := s.Service.Store.ImportDecisionsPage(ctx, afterID, limit+1, time.Now().UTC())
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

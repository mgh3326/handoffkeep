package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	allowed := map[string]bool{"limit": true, "after": true, "after_at": true, "after_id": true, "project": true, "max_task_id": true, "task_ids": true}
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
		if q.Has("after_at") || q.Has("after_id") || q.Has("project") || q.Has("max_task_id") {
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
		if q.Has("after") || q.Has("max_task_id") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 999 {
			limit = 999
		}
		if q.Has("after_at") {
			at, id, err := importCursor(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			var project *string
			if q.Has("project") {
				p := q.Get("project")
				if p == "" {
					writeError(w, http.StatusBadRequest, "invalid_context")
					return
				}
				if p == "/shared" {
					jsonOut(w, http.StatusOK, map[string]any{"items": []any{}, "next_cursor": nil})
					return
				}
				if p == "/unassigned" {
					p = ""
				}
				project = &p
			}
			rows, err := s.Service.Store.ImportTasksUpdatedPage(ctx, project, at, id, limit+1)
			if err != nil {
				appErr(w, r, err)
				return
			}
			var next any
			if len(rows) > limit {
				rows = rows[:limit]
				last := rows[len(rows)-1]
				next = map[string]any{"after_at": last.ImportUpdatedAt, "after_id": last.ID}
			}
			jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
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
			if p == "/shared" {
				jsonOut(w, http.StatusOK, map[string]any{"items": []any{}, "next_cursor": nil})
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
		importRows, err := s.Service.Store.EnrichImportTasks(ctx, rows)
		if err != nil {
			appErr(w, r, err)
			return
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": importRows, "next_cursor": next})
	case "comments":
		if q.Has("after") || q.Has("project") != q.Has("max_task_id") || (q.Has("project") && q.Get("project") == "") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit >= store.TaskCommentListMax {
			limit = store.TaskCommentListMax - 1
		}
		if q.Has("after_at") || q.Has("task_ids") {
			at, id := time.Time{}, int64(0)
			var err error
			if q.Has("after_at") {
				at, id, err = importCursor(r)
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_context")
					return
				}
			} else if q.Has("after_id") {
				id, err = queryCursor(r, "after_id")
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_context")
					return
				}
			}
			var ids []int64
			if q.Has("task_ids") && q.Get("task_ids") == "" {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			if raw := q.Get("task_ids"); raw != "" {
				for _, part := range strings.Split(raw, ",") {
					n, e := strconv.ParseInt(part, 10, 64)
					if e != nil || n <= 0 {
						writeError(w, http.StatusBadRequest, "invalid_context")
						return
					}
					ids = append(ids, n)
				}
			}
			rows, err := s.Service.Store.ImportCommentsUpdatedPage(ctx, ids, at, id, limit+1)
			if err != nil {
				appErr(w, r, err)
				return
			}
			var next any
			if len(rows) > limit {
				rows = rows[:limit]
				last := rows[len(rows)-1]
				next = map[string]any{"after_at": last.CreatedAt, "after_id": last.ID}
			}
			jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
			return
		}
		after := int64(0)
		if q.Has("after_id") {
			after, err = queryCursor(r, "after_id")
			if err != nil {
				appErr(w, r, err)
				return
			}
		}
		project := q.Get("project")
		maxTaskID := int64(0)
		if q.Has("max_task_id") {
			maxTaskID, err = queryCursor(r, "max_task_id")
			if err != nil || maxTaskID == 0 {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
		}
		rows, err := s.Service.Store.ImportCommentsPage(ctx, after, limit, project, maxTaskID)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) == limit {
			next = rows[len(rows)-1].ID
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "comment-authors":
		if q.Has("after_at") || q.Has("after_id") || q.Has("project") || q.Has("max_task_id") || q.Has("task_ids") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 999 {
			limit = 999
		}
		rows, err := s.Service.Store.ImportCommentAuthorsPage(ctx, q.Get("after"), limit+1)
		if err != nil {
			appErr(w, r, err)
			return
		}
		var next any
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].Author
		}
		jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
	case "documents":
		if q.Has("after") || q.Has("max_task_id") || !q.Has("project") || q.Get("project") == "" {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 20 {
			limit = 20
		}
		if q.Has("after_at") {
			at, id, err := importCursor(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			rows, err := s.Service.Store.ImportDocumentsUpdatedPage(ctx, q.Get("project"), at, id, limit+1)
			if err != nil {
				appErr(w, r, err)
				return
			}
			var next any
			if len(rows) > limit {
				rows = rows[:limit]
				last := rows[len(rows)-1]
				next = map[string]any{"after_at": last.UpdatedAt, "after_id": last.ID}
			}
			jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
			return
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
	case "task-documents":
		if q.Has("after") || q.Has("after_at") || q.Has("project") || q.Has("max_task_id") || !q.Has("task_ids") || q.Get("task_ids") == "" {
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
		ids := make([]int64, 0)
		for _, raw := range strings.Split(q.Get("task_ids"), ",") {
			id, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || id <= 0 {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			ids = append(ids, id)
		}
		rows, err := s.Service.Store.ImportTaskDocumentsPage(ctx, ids, afterID, limit+1)
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
		if q.Has("after_at") || q.Has("after") || q.Has("max_task_id") || !q.Has("project") || q.Get("project") == "" {
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
		if q.Has("after") || q.Has("project") || q.Has("max_task_id") {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if limit > 999 {
			limit = 999
		}
		if q.Has("after_at") {
			at, id, err := importCursor(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
			rows, err := s.Service.Store.ImportDecisionsUpdatedPage(ctx, at, id, limit+1, time.Now().UTC())
			if err != nil {
				appErr(w, r, err)
				return
			}
			var next any
			if len(rows) > limit {
				rows = rows[:limit]
				last := rows[len(rows)-1]
				next = map[string]any{"after_at": last.UpdatedAt, "after_id": last.ID}
			}
			jsonOut(w, http.StatusOK, map[string]any{"items": rows, "next_cursor": next})
			return
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

func importCursor(r *http.Request) (time.Time, int64, error) {
	q := r.URL.Query()
	raw := q.Get("after_at")
	if raw == "" {
		return time.Time{}, 0, strconv.ErrSyntax
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, 0, err
	}
	id := int64(0)
	if q.Has("after_id") {
		id, err = queryCursor(r, "after_id")
		if err != nil {
			return time.Time{}, 0, err
		}
	}
	return at.UTC(), id, nil
}

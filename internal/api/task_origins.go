package api

import (
	"context"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// AddTaskOrigin records an origin of a task. The store validates it.
func (s Service) AddTaskOrigin(ctx context.Context, id int64, client string, in store.TaskOriginInput) (store.TaskOrigin, bool, error) {
	return s.Store.AddTaskOrigin(ctx, id, client, in)
}

// ListTaskOrigins lists a task's origins.
func (s Service) ListTaskOrigins(ctx context.Context, id int64) ([]store.TaskOrigin, error) {
	return s.Store.ListTaskOrigins(ctx, id)
}

// FindTaskOrigins looks origins up by the provider's durable id.
func (s Service) FindTaskOrigins(ctx context.Context, provider, externalID string, limit int) ([]store.TaskOrigin, error) {
	return s.Store.FindTaskOrigins(ctx, provider, externalID, limit)
}

// taskOriginAdd answers 201 when the origin is new and 200 when the task
// already had it (its display number or title may have been refreshed).
func (s Server) taskOriginAdd(w http.ResponseWriter, r *http.Request) {
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
	var in store.TaskOriginInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	origin, created, err := s.Service.AddTaskOrigin(r.Context(), id, client, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"origin": origin, "created": created})
}

func (s Server) taskOriginList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.ListTaskOrigins(r.Context(), id)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"origins": xs})
}

// taskOriginFind is the UUID lookup: GET /v1/task-origins?provider=P&external_id=X.
func (s Server) taskOriginFind(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	n, err := queryLimit(r, 100, store.OriginListMax)
	if err != nil || q.Get("provider") == "" || q.Get("external_id") == "" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.FindTaskOrigins(r.Context(), q.Get("provider"), q.Get("external_id"), n)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"origins": xs})
}

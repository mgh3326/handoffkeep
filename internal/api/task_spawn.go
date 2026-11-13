package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// PutTaskSpawn records a task's spawn request (hk 4377) for the service.
func (s Service) PutTaskSpawn(ctx context.Context, id int64, in store.TaskSpawnInput, author string) (store.TaskSpawn, bool, error) {
	return s.Store.PutTaskSpawn(ctx, id, in, author)
}

// TaskStates answers the states query for the service.
func (s Service) TaskStates(ctx context.Context, ids []int64, until string) ([]store.TaskStateEntry, error) {
	return s.Store.TaskStates(ctx, ids, until)
}

// taskSpawnPut answers PUT /v1/tasks/{id}/spawn with the stored record. A write
// not newer than the stored record applies nothing and still answers 200 with
// that record, so a replay is harmless.
func (s Server) taskSpawnPut(w http.ResponseWriter, r *http.Request) {
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
	var in store.TaskSpawnInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.PutTaskSpawn(r.Context(), id, in, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// taskStates answers GET /v1/tasks/states?ids=1,2,3[&until=merged|deployed]
// with one entry per task that exists, in the order asked.
func (s Server) taskStates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	ids, err := parseTaskIDList(r.URL.Query().Get("ids"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	out, err := s.Service.TaskStates(r.Context(), ids, r.URL.Query().Get("until"))
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

// parseTaskIDList reads a comma-separated list of positive task ids. It
// refuses an empty list, a non-number and an id below 1; the count bound is
// the store's.
func parseTaskIDList(raw string) ([]int64, error) {
	if raw == "" {
		return nil, errors.New("ids is required")
	}
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id < 1 {
			return nil, errors.New("ids must be task ids")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

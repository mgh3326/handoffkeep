package api

import (
	"context"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// AddTaskWait records a hand wait of a task for the service (hk 5128).
func (s Service) AddTaskWait(ctx context.Context, id int64, in store.TaskWaitInput, author string) (store.TaskWait, error) {
	return s.Store.AddTaskWait(ctx, id, in, author)
}

// ClearTaskWait ends a hand wait of a task for the service.
func (s Service) ClearTaskWait(ctx context.Context, id, waitID int64, by, note string) (store.TaskWait, bool, error) {
	return s.Store.ClearTaskWait(ctx, id, waitID, by, note)
}

// ListWaits reads the waits sections at now for the service.
func (s Service) ListWaits(ctx context.Context, now time.Time, section string) (store.WaitsView, error) {
	return s.Store.ListWaits(ctx, now, section)
}

// taskWaitAdd answers POST /v1/tasks/{id}/waits with the open wait it records.
// Repeating an open wait of the same kind and ref updates its reason.
func (s Server) taskWaitAdd(w http.ResponseWriter, r *http.Request) {
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
	var in store.TaskWaitInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	out, err := s.Service.AddTaskWait(r.Context(), id, in, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

// taskWaitClear answers POST /v1/tasks/{id}/waits/{wait}/clear with the wait and
// changed: whether this call ended it. Clearing a wait that has already ended
// changes nothing.
func (s Server) taskWaitClear(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	waitID, err := pathID(r, "wait")
	if err != nil || waitID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var in struct {
		Note string `json:"note"`
	}
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	wait, changed, err := s.Service.ClearTaskWait(r.Context(), id, waitID, client, in.Note)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, struct {
		Changed bool           `json:"changed"`
		Wait    store.TaskWait `json:"wait"`
	}{changed, wait})
}

// waitsList answers GET /v1/waits?section=<key>&open=true with the four waits
// sections and the ordinary backlog at the read time. section filters the rows
// to one section; open must be true when given, since every row is open.
func (s Server) waitsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	query := r.URL.Query()
	if open := query.Get("open"); open != "" && open != "true" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	out, err := s.Service.ListWaits(r.Context(), time.Now().UTC(), query.Get("section"))
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

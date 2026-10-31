package api

import (
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// jobHostAllowed enforces the job host rule from the start (unlike presence,
// there is no legacy warn mode): the client must have a HANDOFFKEEP_HOSTS_
// line and the line must list host.
func (s Server) jobHostAllowed(client, host string) bool {
	allowed, listed := s.HostGrants.Allows(client, host)
	return listed && allowed
}

// jobClaim is POST /v1/jobs: {job_id, task_id, host} claims an id on the
// executing host. An identical replay answers 200, a different claim on a
// taken id answers 409 job_id_taken.
func (s Server) jobClaim(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input struct {
		JobID  string `json:"job_id"`
		TaskID int64  `json:"task_id"`
		Host   string `json:"host"`
	}
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	if !store.ValidJobID(input.JobID) || !store.ValidJobHost(input.Host) || input.TaskID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if !s.jobHostAllowed(client, input.Host) {
		writeError(w, http.StatusForbidden, "job_host_forbidden")
		return
	}
	j, created, err := s.Service.Store.ClaimJob(r.Context(), store.Job{
		JobID: input.JobID, TaskID: input.TaskID, Host: input.Host, CreatedBy: client,
	})
	if err != nil {
		appErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, j)
}

// jobUpdate is PUT /v1/jobs/{job}: merges the allowlisted fields over the
// record. Unknown fields fail decode; the store enforces forward-only
// states, the write-once result and the field allowlist.
func (s Server) jobUpdate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id := r.PathValue("job")
	if !store.ValidJobID(id) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input store.JobUpdate
	if err := decode(r, &input, store.JobResultMaxBytes); err != nil {
		appErr(w, err)
		return
	}
	// The record's host decides which clients may write it; the row is
	// fetched here so a forbidden write never reaches the store.
	j, found, err := s.Service.Store.GetJob(r.Context(), id)
	if err != nil {
		appErr(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if !s.jobHostAllowed(client, j.Host) {
		writeError(w, http.StatusForbidden, "job_host_forbidden")
		return
	}
	j, err = s.Service.Store.UpdateJob(r.Context(), id, input)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, j)
}

// jobGet is GET /v1/jobs/{job}.
func (s Server) jobGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id := r.PathValue("job")
	if !store.ValidJobID(id) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	j, found, err := s.Service.Store.GetJob(r.Context(), id)
	if err != nil {
		appErr(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	jsonOut(w, http.StatusOK, j)
}

// jobsList is GET /v1/jobs?task=N.
func (s Server) jobsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	taskID, err := strconv.ParseInt(r.URL.Query().Get("task"), 10, 64)
	if err != nil || taskID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	jobs, err := s.Service.Store.ListJobsByTask(r.Context(), taskID, limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"jobs": jobs})
}

package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// servicePut is PUT /v1/services/{name}: the operator registers or replaces a
// deployable service. Only the operator clients may call it.
func (s Server) servicePut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID && client != operatorDeskClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	var input struct {
		Repo         string   `json:"repo"`
		PathPatterns []string `json:"path_patterns"`
		DeployMode   string   `json:"deploy_mode"`
		Targets      []string `json:"targets"`
	}
	if err := decode(r, &input, bodyEvent); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.PutService(r.Context(), store.Service{Name: r.PathValue("name"), Repo: input.Repo, PathPatterns: input.PathPatterns, DeployMode: input.DeployMode, Targets: input.Targets})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// servicesList is GET /v1/services.
func (s Server) servicesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, err := s.Service.Store.ListServices(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"services": xs})
}

// servicePending is GET /v1/services/{name}/pending: the merged changes the
// service has not yet shipped to every target, computed on read.
func (s Server) servicePending(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.Store.PendingChanges(r.Context(), r.PathValue("name"), limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"service": r.PathValue("name"), "changes": xs})
}

// deployStart is POST /v1/deploys: one attempt with its candidate changes,
// frozen at start. 201 for a new attempt, 200 for a replay of the same one.
func (s Server) deployStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input store.DeployStart
	if err := decode(r, &input, store.MaxDeployBytes); err != nil {
		appErr(w, r, err)
		return
	}
	d, created, err := s.Service.Store.StartDeploy(r.Context(), input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"created": created, "deploy": d})
}

// deployFinish is POST /v1/deploys/{attempt_id}/finish.
func (s Server) deployFinish(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input store.DeployFinish
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	d, err := s.Service.Store.FinishDeploy(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, d)
}

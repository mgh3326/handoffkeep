package api

import (
	"errors"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// projectOwnerError is the owner routes' one translation from the store. An
// unknown project answers the shared not_found (AGENTS.md lets a x_not_found
// sentinel do that); the store keeps ErrProjectNotFound so its own callers see
// one sentinel for "no such project".
func projectOwnerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrProjectNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	appErr(w, r, err)
}

// projectOwnersList answers which design session owns each project. Every row
// carries design_lane and the resolved effective_owner, so a caller never
// repeats the operator-desk fallback rule.
func (s Server) projectOwnersList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	owners, err := s.Service.Store.ListProjectOwners(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"projects": owners})
}

// projectOwner reads one project's owner with its last 20 events, and sets or
// clears it. The actor is the authenticated client name; the body never
// supplies one.
func (s Server) projectOwner(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	if r.Method == http.MethodGet {
		owner, err := s.Service.Store.GetProjectOwner(r.Context(), project)
		if err != nil {
			projectOwnerError(w, r, err)
			return
		}
		events, err := s.Service.Store.ProjectOwnerHistory(r.Context(), project, 20)
		if err != nil {
			appErr(w, r, err)
			return
		}
		jsonOut(w, http.StatusOK, store.ProjectOwnerDetail{ProjectOwner: owner, Events: events})
		return
	}
	var in struct {
		DesignLane string `json:"design_lane"`
		Note       string `json:"note"`
	}
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	owner, err := s.Service.Store.SetProjectDesignLane(r.Context(), project, in.DesignLane, client, in.Note)
	if err != nil {
		projectOwnerError(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, owner)
}

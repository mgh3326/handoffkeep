package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (s Server) projectMetadata(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	name := r.PathValue("project")
	if r.Method == http.MethodGet {
		p, err := s.Service.Store.GetProjectMetadata(r.Context(), name)
		if err != nil {
			appErr(w, r, err)
			return
		}
		jsonOut(w, http.StatusOK, p)
		return
	}
	if r.Method == http.MethodPatch {
		var patch store.ProjectMetadataPatch
		if err := decode(r, &patch, bodySmall); err != nil {
			appErr(w, r, err)
			return
		}
		p, err := s.Service.Store.PatchProjectMetadata(r.Context(), name, patch)
		if err != nil {
			appErr(w, r, err)
			return
		}
		jsonOut(w, http.StatusOK, p)
		return
	}
	var p store.ProjectMetadata
	if err := decode(r, &p, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	p.Name = name
	p, err := s.Service.Store.SetProjectMetadata(r.Context(), p)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, p)
}
func (s Server) milestoneSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	var m store.ProjectMilestone
	if err := decode(r, &m, bodyLarge); err != nil {
		appErr(w, r, err)
		return
	}
	if r.Method == http.MethodPut {
		id, err := pathID(r, "milestone")
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		m.ID = id
	} else {
		m.ID = 0
	}
	m, err := s.Service.Store.SaveMilestone(r.Context(), m)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, m)
}
func (s Server) taskMilestone(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var in struct {
		MilestoneID *int64 `json:"milestone_id"`
	}
	if err = decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.Store.SetTaskMilestone(r.Context(), id, in.MilestoneID, by)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) milestoneGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "milestone")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	m, err := s.Service.Store.GetMilestone(r.Context(), id)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, m)
}

func (s Server) milestonePatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "milestone")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var p store.MilestonePatch
	if err = decode(r, &p, bodyLarge); err != nil {
		appErr(w, r, err)
		return
	}
	m, err := s.Service.Store.PatchMilestone(r.Context(), id, p)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, m)
}

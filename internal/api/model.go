package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func modelID(r *http.Request, key string) (int64, error) {
	return strconv.ParseInt(r.PathValue(key), 10, 64)
}

func (s Server) taskDue(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var in struct {
		DueAt *time.Time `json:"due_at"`
	}
	if err = decode(r, &in, 4096); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.Store.SetTaskDue(r.Context(), id, in.DueAt, by)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

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
		if err := decode(r, &patch, 4096); err != nil {
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
	if err := decode(r, &p, 4096); err != nil {
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
	if err := decode(r, &m, 65536); err != nil {
		appErr(w, r, err)
		return
	}
	if r.Method == http.MethodPut {
		id, err := modelID(r, "milestone")
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
func (s Server) labelsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	labels, err := s.Service.Store.ListLabels(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"labels": labels})
}
func (s Server) labelSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	var l store.Label
	if err := decode(r, &l, 4096); err != nil {
		appErr(w, r, err)
		return
	}
	if r.Method == http.MethodPut {
		id, err := modelID(r, "label")
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		l.ID = id
	} else {
		l.ID = 0
	}
	l, err := s.Service.Store.SaveLabel(r.Context(), l)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, l)
}
func (s Server) labelRetire(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := modelID(r, "label")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if err = s.Service.Store.RetireLabel(r.Context(), id); err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"retired": true})
}
func (s Server) taskMilestone(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var in struct {
		MilestoneID *int64 `json:"milestone_id"`
	}
	if err = decode(r, &in, 4096); err != nil {
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
func (s Server) taskLabel(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var in struct {
		LabelID int64 `json:"label_id"`
		Attach  *bool `json:"attach"`
	}
	if err = decode(r, &in, 4096); err != nil {
		appErr(w, r, err)
		return
	}
	if in.Attach == nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	x, _, err := s.Service.Store.SetTaskLabel(r.Context(), id, in.LabelID, by, *in.Attach)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}
func (s Server) taskDeadlines(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var d store.TaskDeadlines
	if err = decode(r, &d, 4096); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.Store.SetTaskDeadlines(r.Context(), id, d, by)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}
func (s Server) taskSubscriber(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var in struct {
		Subscriber string `json:"subscriber"`
		Source     string `json:"source"`
		Active     *bool  `json:"active"`
	}
	if err = decode(r, &in, 4096); err != nil {
		appErr(w, r, err)
		return
	}
	if in.Active == nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	x, _, err := s.Service.Store.SetTaskSubscriber(r.Context(), id, in.Subscriber, in.Source, by, *in.Active)
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
	id, err := modelID(r, "milestone")
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
	id, err := modelID(r, "milestone")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	var p store.MilestonePatch
	if err = decode(r, &p, 65536); err != nil {
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

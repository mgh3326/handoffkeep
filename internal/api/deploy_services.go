package api

import (
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
)

func (s Server) deployServicesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, err := s.Service.Store.ListDeployServices(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"services": xs})
}
func (s Server) deployServiceWrite(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID && client != operatorDeskClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	var x store.DeployService
	var err error
	if r.Method == http.MethodPut {
		defer r.Body.Close()
		var in struct {
			Repo           string   `json:"repo"`
			RecordPrefixes []string `json:"record_prefixes"`
		}
		if err = decode(r, &in, bodySmall); err != nil {
			appErr(w, r, err)
			return
		}
		x, err = s.Service.Store.PutDeployService(r.Context(), store.DeployService{Name: r.PathValue("name"), Repo: in.Repo, RecordPrefixes: in.RecordPrefixes})
	} else {
		x, err = s.Service.Store.DeactivateDeployService(r.Context(), r.PathValue("name"))
	}
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

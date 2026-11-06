package api

import (
	"net/http"
	"time"
)

func (s Server) projectFrontier(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	if r.URL.Query().Has("since") {
		var err error
		since, err = time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		if err != nil || since.Before(now.Add(-30*24*time.Hour)) || since.After(now) {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	result, err := s.Service.Store.ProjectFrontier(r.Context(), r.PathValue("name"), since, now)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, result)
}

package api

import "net/http"

func (s Server) linearMetadataRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.LinearMetadata == nil {
		writeError(w, http.StatusServiceUnavailable, "linear_metadata_disabled")
		return
	}
	var body struct{}
	if err := decode(r, &body, 1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if err := s.LinearMetadata.RefreshLinearMetadata(r.Context()); err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

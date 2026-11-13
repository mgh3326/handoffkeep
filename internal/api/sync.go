package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func syncError(w http.ResponseWriter, r *http.Request, err error) {
	var reset *store.SyncReset
	if errors.As(err, &reset) {
		jsonOut(w, http.StatusGone, map[string]any{"reset": true, "min_sync_id": reset.MinSyncID})
		return
	}
	appErr(w, r, err)
}
func (s Server) syncDelta(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	after, err := queryCursor(r, "after")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	limit, err := queryLimit(r, 100, store.SyncLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	changes, meta, err := s.Service.Store.SyncDelta(r.Context(), after, limit)
	if err != nil {
		syncError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	for _, change := range changes {
		if err := enc.Encode(change); err != nil {
			return
		}
	}
	_ = enc.Encode(map[string]any{"_metadata_": meta})
}

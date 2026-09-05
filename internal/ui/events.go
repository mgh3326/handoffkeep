package ui

import (
	"net/http"
	"time"
)

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	relayMaxID, taskEventMaxID, err := h.store.EventWatermarks(r.Context())
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	writeSSE(w, relayMaxID, taskEventMaxID)

	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			nextRelay, nextTaskEvent, err := h.store.EventWatermarks(r.Context())
			if err != nil {
				continue
			}
			if nextRelay != relayMaxID || nextTaskEvent != taskEventMaxID {
				relayMaxID, taskEventMaxID = nextRelay, nextTaskEvent
				writeSSE(w, relayMaxID, taskEventMaxID)
			}
		}
	}
}

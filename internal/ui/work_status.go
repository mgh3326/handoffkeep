package ui

import (
	"log"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

// workStatus serves GET /ui/api/work-status: open task counts per project and state group,
// in-work and backlog lists, movement over today and the last 7 days, and deploy-waiting tasks.
func (h *Handler) workStatus(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	status, err := h.store.ReadWorkStatus(r.Context(), time.Now().UTC())
	if err != nil {
		log.Printf("ui.work_status_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	writeBoardJSON(w, status)
}

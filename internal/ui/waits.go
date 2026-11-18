package ui

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// waits serves GET /ui/api/waits?section=<key>: the waits sections and the
// ordinary backlog at the read time (hk 5128). It is read-only; the page has no
// action that writes a wait or answers a decision.
func (h *Handler) waits(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	view, err := h.store.ListWaits(r.Context(), time.Now().UTC(), r.URL.Query().Get("section"))
	if errors.Is(err, store.ErrTaskWaitInvalid) {
		writeJSONError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if err != nil {
		log.Printf("ui.waits_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeBoardJSON(w, view)
}

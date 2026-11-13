package ui

import (
	"log"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// workStatus serves GET /ui/api/work-status: open task counts per project and state group,
// in-work and backlog lists, movement over today and the last 7 days, and deploy-waiting tasks.
func (h *Handler) workStatus(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	now := time.Now().UTC()
	if h.ytTasks != nil {
		h.workStatusYouTrack(w, r, now)
		return
	}
	status, err := h.store.ReadWorkStatus(r.Context(), now)
	if err != nil {
		log.Printf("ui.work_status_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	writeBoardJSON(w, status)
}

// workStatusYouTrack builds the work status from YouTrack's task issues and
// YouTrack's state changes (state entry and close times) and hk's overlay
// (jobs, hosts, PRs, bundle parents, deploy records, queued spawns) for the
// tasks the page lists.
func (h *Handler) workStatusYouTrack(w http.ResponseWriter, r *http.Request, now time.Time) {
	snap, err := h.ytTasks.snapshot(r.Context())
	if err != nil {
		youTrackReadFailed(w, r, err)
		return
	}
	listed := map[string]bool{}
	for _, state := range store.WorkStatusStates {
		listed[state] = true
	}
	ids := []int64{}
	for _, t := range snap.Tasks {
		if t.StateKnown && listed[t.State] {
			ids = append(ids, t.TaskID)
		}
	}
	hist, err := h.ytTasks.history(r.Context())
	if err != nil {
		youTrackReadFailed(w, r, err)
		return
	}
	overlay, err := h.store.ReadWorkStatusOverlay(r.Context(), ids)
	if err != nil {
		youTrackReadFailed(w, r, err)
		return
	}
	writeBoardJSON(w, youTrackWorkStatus(now, snap, hist, overlay))
}

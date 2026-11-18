package api

import (
	"net/http"
	"time"
)

// dotsInboxReads is GET /v1/dots/inbox-reads?since=<RFC3339>&limit=N: the
// recorded dots_inbox MCP calls, newest first (hk 4924).
func (s Server) dotsInboxReads(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		since = parsed.UTC()
	}
	n, err := queryLimit(r, 50, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	xs, err := s.Service.Store.ListDotsInboxReads(r.Context(), since, n)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"reads": xs})
}

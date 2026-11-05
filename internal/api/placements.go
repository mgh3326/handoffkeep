package api

import (
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// placementNode requires a node credential bound to exactly one host. Broad
// operator credentials cannot manufacture the decided_on audit identity.
func (s Server) placementNode(client string) (string, bool) {
	hosts := s.HostGrants[client]
	if len(hosts) != 1 {
		return "", false
	}
	for host, allowed := range hosts {
		return host, allowed
	}
	return "", false
}

func (s Server) placementCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	host, ok := s.placementNode(client)
	if !ok {
		writeError(w, http.StatusForbidden, "placement_node_required")
		return
	}
	defer r.Body.Close()
	var input store.PlacementDecision
	if err := decode(r, &input, bodyLarge); err != nil {
		appErr(w, r, err)
		return
	}
	input.ID, input.DecidedOn = 0, host
	result, created, err := s.Service.Store.RecordPlacement(r.Context(), input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, result)
}

func (s Server) placementsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	since := time.Now().UTC().Add(-store.PlacementRetention)
	if value := r.URL.Query().Get("since"); value != "" {
		since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	rows, err := s.Service.Store.ListPlacements(r.Context(), r.URL.Query().Get("job"), r.URL.Query().Get("host"), since, limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"placements": rows})
}

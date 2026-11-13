package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
)

// capabilityList answers GET /v1/capabilities with the protocol version and the
// request shapes this build accepts. A client reads it once and sends a shape
// only when it is listed. A server that predates the route answers 404.
func (s Server) capabilityList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"protocol_version": capabilities.ProtocolVersion,
		"features":         capabilities.Features,
	})
}

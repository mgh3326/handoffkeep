package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (s Server) dotsRequestCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var in store.DotsRequestInput
	if err := decode(r, &in, bodyLarge); err != nil {
		appErr(w, r, err)
		return
	}
	task, err := s.Service.Store.CreateDotsRequest(r.Context(), client, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusCreated, map[string]any{"task": task})
}

// dotsRequestCancel is POST /v1/dots/requests/{id}/cancel: the request's
// requester lane settles it as canceled with a reason (hk 4987).
func (s Server) dotsRequestCancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var in store.DotsCancelInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	task, err := s.Service.Store.CancelDotsRequest(r.Context(), id, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"task": task})
}

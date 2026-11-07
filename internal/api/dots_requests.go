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

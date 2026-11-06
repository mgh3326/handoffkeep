package api

import (
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
	"time"
)

func (s Server) standingPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID && client != operatorDeskClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	var in store.StandingOrderInput
	if err := decode(r, &in, bodyStanding); err != nil {
		appErr(w, r, err)
		return
	}
	order, created, err := s.Service.Store.PutStandingOrder(r.Context(), in, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	if created {
		jsonOut(w, http.StatusCreated, order)
		return
	}
	jsonOut(w, http.StatusOK, struct {
		store.StandingOrder
		Duplicate bool `json:"duplicate"`
	}{order, true})
}
func (s Server) standingList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	orders, truncated, err := s.Service.Store.ListStandingOrders(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("lane"))
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"generated_at": time.Now().UTC(), "orders": orders, "truncated": truncated})
}

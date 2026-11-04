package api

import (
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ledger serves GET /v1/ledger?consumer=<name>: the desk ledger, five fixed
// sections in order. Reading never moves the consumer's cursor. A cursor past
// the change feed's reset boundary answers 410 {"reset":true,"min_sync_id":N}
// as the feed does.
func (s Server) ledger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	consumer := r.URL.Query().Get("consumer")
	if consumer == "" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	out, err := s.Service.Store.Ledger(r.Context(), consumer, time.Now().UTC())
	if err != nil {
		syncError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, http.StatusOK, out)
}

type ledgerAckInput struct {
	Consumer string `json:"consumer"`
	SyncID   *int64 `json:"sync_id"`
}

// ledgerAck serves POST /v1/ledger/ack {consumer, sync_id}: the only way a
// cursor moves, and only forward.
func (s Server) ledgerAck(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input ledgerAckInput
	if err := decode(r, &input, 1024); err != nil {
		appErr(w, r, err)
		return
	}
	if input.Consumer == "" || input.SyncID == nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	c, err := s.Service.Store.AckLedger(r.Context(), input.Consumer, *input.SyncID, by, time.Now().UTC())
	if err != nil {
		syncError(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, c)
}

type statusUpdateInput struct {
	Health string `json:"health"`
	Body   string `json:"body"`
}

// statusUpdates serves POST and GET /v1/projects/{name}/status-updates.
func (s Server) statusUpdates(w http.ResponseWriter, r *http.Request) {
	by, ok := s.auth(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if r.Method == http.MethodGet {
		limit, err := queryLimit(r, 100, 1000)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		out, err := s.Service.Store.ListStatusUpdates(r.Context(), name, limit, time.Now().UTC())
		if err != nil {
			appErr(w, r, err)
			return
		}
		jsonOut(w, http.StatusOK, out)
		return
	}
	defer r.Body.Close()
	var input statusUpdateInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.CreateStatusUpdate(r.Context(), name, input.Health, input.Body, by, time.Now().UTC())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusCreated, x)
}

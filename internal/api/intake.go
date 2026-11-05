package api

import (
	"context"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// IntakeTask puts one item into triage for the authenticated client.
func (s Service) IntakeTask(ctx context.Context, client string, in store.IntakeInput) (store.Task, bool, error) {
	in.CreatedBy = client
	return s.Store.IntakeTask(ctx, in)
}

// EmitTicketSet records a ticket set for the authenticated client.
func (s Service) EmitTicketSet(ctx context.Context, client string, in store.TicketSetInput) (store.TicketSet, bool, error) {
	in.CreatedBy = client
	return s.Store.EmitTicketSet(ctx, in)
}

// ListTriage returns the triage queue with each ticket set as one row.
func (s Service) ListTriage(ctx context.Context, limit int) ([]store.TriageRow, error) {
	return s.Store.ListTriage(ctx, limit)
}

// ticketSetRequestMax bounds a ticket set body: a parent and
// store.TicketSetMaxChildren children of a few KiB each, with room for JSON
// escaping.
const ticketSetRequestMax = 4 << 20

// intake answers POST /v1/intake: 201 with created:true for a new triage
// task, 200 with created:false and the recorded task for a repeated
// (source, source_ref).
func (s Server) intake(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var in store.IntakeInput
	if err := decode(r, &in, store.DocumentMaxBytes+store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, created, err := s.Service.IntakeTask(r.Context(), client, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"created": created, "task": x})
}

// ticketSetsCreate answers POST /v1/ticket-sets: 201 for a new set, 200 with
// the same ids for a repeated payload, 409 ticket_set_conflict for a changed
// one.
func (s Server) ticketSetsCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var in store.TicketSetInput
	if err := decodeLimited(r.Body, &in, ticketSetRequestMax); err != nil {
		appErr(w, r, err)
		return
	}
	set, created, err := s.Service.EmitTicketSet(r.Context(), client, in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, struct {
		store.TicketSet
		Created bool `json:"created"`
	}{set, created})
}

// triageList answers GET /v1/triage: the triage queue, oldest first, with a
// ticket set as one row.
func (s Server) triageList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	rows, err := s.Service.ListTriage(r.Context(), limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"triage": rows})
}

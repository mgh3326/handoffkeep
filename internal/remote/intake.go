package remote

import (
	"context"
	"fmt"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Intake puts one item into triage. created is false when the server already
// held a task for the item's (source, source_ref) and returned it.
func (c Client) Intake(ctx context.Context, in store.IntakeInput) (store.Task, bool, error) {
	var out struct {
		Created *bool      `json:"created"`
		Task    store.Task `json:"task"`
	}
	if err := c.call(ctx, "POST", "/v1/intake", in, &out); err != nil {
		return store.Task{}, false, err
	}
	if out.Created == nil || out.Task.ID < 1 {
		return store.Task{}, false, fmt.Errorf("intake_response_malformed: server response has no created flag or task id")
	}
	return out.Task, *out.Created, nil
}

// EmitTicketSet records a ticket set. created is false when the same payload
// was already recorded; a changed payload fails with ticket_set_conflict.
func (c Client) EmitTicketSet(ctx context.Context, in store.TicketSetInput) (store.TicketSet, bool, error) {
	var out struct {
		store.TicketSet
		Created *bool `json:"created"`
	}
	if err := c.call(ctx, "POST", "/v1/ticket-sets", in, &out); err != nil {
		return store.TicketSet{}, false, err
	}
	if out.Created == nil || out.ParentID < 1 || len(out.Children) != len(in.Children) {
		return store.TicketSet{}, false, fmt.Errorf("ticket_set_response_malformed: server response has no created flag, parent id or child ids")
	}
	return out.TicketSet, *out.Created, nil
}

// ListTriage reads the triage queue, oldest first, with each ticket set as
// one row.
func (c Client) ListTriage(ctx context.Context, limit int) ([]store.TriageRow, error) {
	var out struct {
		Triage []store.TriageRow `json:"triage"`
	}
	if err := c.call(ctx, "GET", fmt.Sprintf("/v1/triage?limit=%d", limit), nil, &out); err != nil {
		return nil, err
	}
	if out.Triage == nil {
		return nil, fmt.Errorf("triage_response_malformed: server response has no triage list")
	}
	return out.Triage, nil
}

package api

import (
	"net/http"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Task orders (hk 5125, docs/task-orders.md). Only the operator bearer writes
// an order or withdraws one: on /v1 that client is the verified operator path,
// the same one a decision answer needs. A dots relay arrives through MCP, and
// the console through its own Access-authenticated route; both call the same
// store methods. Any bearer client acknowledges an order, as the lane's act.

type taskOrderInput struct {
	Words          string `json:"words"`
	IdempotencyKey string `json:"idempotency_key"`
}

type taskOrderAckInput struct {
	Comment string `json:"comment"`
}

type taskOrderWithdrawInput struct {
	Reason string `json:"reason"`
}

// taskOrderStatus answers a new order 201 and a replay 200.
func taskOrderStatus(out store.TaskOrderResult) int {
	if out.Duplicate {
		return http.StatusOK
	}
	return http.StatusCreated
}

func (s Server) taskOrderCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskOrderInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	out, err := s.Service.Store.CreateTaskOrder(r.Context(), store.CreateTaskOrderInput{
		TaskID: id, Words: input.Words, Actor: client, Channel: store.TaskOrderChannelCLI, Verified: true, Key: input.IdempotencyKey,
	})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, taskOrderStatus(out), out)
}

func (s Server) taskOrdersList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	open := r.URL.Query().Get("open")
	if open != "" && open != "1" && open != "true" && open != "0" && open != "false" {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	orders, err := s.Service.Store.ListTaskOrders(r.Context(), id, open == "1" || open == "true")
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"orders": orders})
}

func (s Server) taskOrderAck(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input taskOrderAckInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	out, err := s.Service.Store.AckTaskOrder(r.Context(), r.PathValue("order"), client, input.Comment)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

func (s Server) taskOrderWithdraw(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	var input taskOrderWithdrawInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	out, err := s.Service.Store.WithdrawTaskOrder(r.Context(), r.PathValue("order"), client, strings.TrimSpace(input.Reason))
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

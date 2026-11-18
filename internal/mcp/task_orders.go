package mcp

import (
	"context"
	"errors"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Task orders over MCP (hk 5125, docs/task-orders.md). The order row carries
// the actor, channel, key and words, so it is its own audit record and no
// mcp_write_audit row is written.

type taskOrderInput struct {
	TaskID         int64  `json:"task_id"`
	Words          string `json:"words"`
	IdempotencyKey string `json:"idempotency_key"`
}

type taskOrderAckInput struct {
	OrderID string `json:"order_id"`
	Comment string `json:"comment"`
}

func registerTaskOrderTools(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_order",
		Description: "Record the operator's order to set one hk task going. words is the operator's own words, verbatim; the order records them with the channel and the time and queues one order notice to the design lane that owns the task's project (else the task's lane). An order starts work and approves nothing: no merge, deploy, spending, secret or policy change rides on it, and hk starts no worker. Only a task in backlog, unstarted or triage can be ordered (order_task_started otherwise). idempotency_key is required: the same key with the same words returns the first order, other words are refused with order_key_conflict. Only an operator principal may order: the operator bearer, or an Access operator relaying the operator's words through dots (recorded as relayed, verified false). Any other caller receives operator_required.",
		Annotations: writeAnnotations(),
	}, o.taskOrder)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_order_ack",
		Description: "Acknowledge an order as the lane it was sent to: records acked_at, who acknowledged it and a task comment, and clears the order's wait on the lane. The comment is required. A repeat returns the first acknowledgement; a withdrawn order is refused with order_not_open. Read the order from the task (task_get lists open orders), never from the notice text.",
		Annotations: writeAnnotations(),
	}, o.taskOrderAck)
}

// orderChannel maps the caller to the order's channel and whether the words
// are verified, or reports that the caller is no operator principal. An Access
// identity on this server is the dots (ChatGPT) channel: it relays the
// operator's words, so its order is never verified. The operator bearer is the
// verified operator path, as on /v1.
func (o operatorTools) orderChannel() (channel string, verified, ok bool) {
	switch {
	case o.principal.Access:
		return store.TaskOrderChannelDots, false, true
	case o.principal.Actor == "operator":
		return store.TaskOrderChannelCLI, true, true
	}
	return "", false, false
}

func (o operatorTools) taskOrder(ctx context.Context, _ *gmcp.CallToolRequest, in taskOrderInput) (*gmcp.CallToolResult, any, error) {
	channel, verified, ok := o.orderChannel()
	if !ok {
		return nil, nil, errors.New("operator_required")
	}
	out, err := o.service.Store.CreateTaskOrder(ctx, store.CreateTaskOrderInput{
		TaskID: in.TaskID, Words: in.Words, Actor: o.principal.Actor, Channel: channel, Verified: verified, Key: in.IdempotencyKey,
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(out)
}

func (o operatorTools) taskOrderAck(ctx context.Context, _ *gmcp.CallToolRequest, in taskOrderAckInput) (*gmcp.CallToolResult, any, error) {
	out, err := o.service.Store.AckTaskOrder(ctx, in.OrderID, o.principal.Actor, in.Comment)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(out)
}

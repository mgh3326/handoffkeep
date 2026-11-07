package mcp

import (
	"context"
	"errors"
	"os"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerDotsTools(s *gmcp.Server, service api.Service, p principal) {
	o := operatorTools{service: service, principal: p}
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_inbox", Description: "List open dots requests oldest first. Pass the last task id as cursor to continue.", Annotations: readOnlyAnnotations()}, o.dotsInbox)
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_reply", Description: "Reply to a dots request. START requires expected_updated_at from the inbox. Use ask_operator for questions requiring an operator decision. Reuse an idempotency key only for the same reply.", Annotations: writeAnnotations()}, o.dotsReply)
}

func (o operatorTools) dotsReply(ctx context.Context, _ *gmcp.CallToolRequest, in store.DotsReplyInput) (*gmcp.CallToolResult, any, error) {
	if os.Getenv("HANDOFFKEEP_DOTS_REPLY_WRITES") != "on" {
		return nil, nil, errors.New("dots_reply_writes_disabled")
	}
	result, err := o.service.Store.ReplyDots(ctx, o.principal.Actor, o.principal.Channel, in)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(result)
}

type dotsInboxInput struct {
	Cursor int64 `json:"cursor,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

func (o operatorTools) dotsInbox(ctx context.Context, _ *gmcp.CallToolRequest, in dotsInboxInput) (*gmcp.CallToolResult, any, error) {
	limit := clampLimit(in.Limit, 20, 100)
	tasks, err := o.service.Store.DotsInbox(ctx, in.Cursor, limit)
	if err != nil {
		return nil, nil, err
	}
	type entry struct {
		Task          store.Task        `json:"task"`
		Body          string            `json:"body"`
		RequesterLane string            `json:"requester_lane"`
		Replies       []store.DotsReply `json:"replies"`
	}
	items := make([]entry, 0, len(tasks))
	for _, task := range tasks {
		doc, found, err := o.service.GetDocument(ctx, task.BodyDoc)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, errors.New("dots request body not found")
		}
		comments, err := o.service.Store.ListDotsReplies(ctx, task.ID)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, entry{Task: task, Body: doc.Body, RequesterLane: task.ParentLane, Replies: comments})
	}
	var next int64
	if len(tasks) == limit {
		next = tasks[len(tasks)-1].ID
	}
	return jsonResult(map[string]any{"requests": items, "next_cursor": next})
}

package mcp

import (
	"context"
	"errors"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerDotsTools(s *gmcp.Server, service api.Service, p principal) {
	o := operatorTools{service: service, principal: p}
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_inbox", Description: "List open dots requests oldest first. Pass the last task id as cursor to continue.", Annotations: readOnlyAnnotations()}, o.dotsInbox)
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_reply", Description: "Reply to a dots request. Use kind START, QUESTION, BLOCKED or RESULT; set ask_operator for questions requiring an operator decision. Reuse an idempotency key only for the same reply.", Annotations: writeAnnotations()}, o.dotsReply)
}

func (o operatorTools) dotsReply(ctx context.Context, _ *gmcp.CallToolRequest, in store.DotsReplyInput) (*gmcp.CallToolResult, any, error) {
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
		Task          store.Task          `json:"task"`
		Body          string              `json:"body"`
		RequesterLane string              `json:"requester_lane"`
		Replies       []store.TaskComment `json:"replies"`
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
		comments := []store.TaskComment{}
		var after int64
		for {
			page, err := o.service.ListTaskComments(ctx, task.ID, after, store.TaskCommentListMax)
			if err != nil {
				return nil, nil, err
			}
			comments = append(comments, page...)
			if len(page) < store.TaskCommentListMax {
				break
			}
			after = page[len(page)-1].ID
		}
		items = append(items, entry{Task: task, Body: doc.Body, RequesterLane: task.ParentLane, Replies: comments})
	}
	var next int64
	if len(tasks) == limit {
		next = tasks[len(tasks)-1].ID
	}
	return jsonResult(map[string]any{"requests": items, "next_cursor": next})
}

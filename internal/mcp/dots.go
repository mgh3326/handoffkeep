package mcp

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// dotsInboxAttachmentLimit caps each attachment list an inbox entry
	// carries. A longer list is cut to this many items and reported truncated.
	dotsInboxAttachmentLimit = 20
	// dotsInboxNamedTaskLimit caps how many tasks one request body may name
	// for referenced_attachments.
	dotsInboxNamedTaskLimit = 5
)

// dotsBodyTaskRef finds a task named as "hk <id>" in a request body, in any
// case. A requester points at the task holding the full package this way, and
// the files on that task are what the request means dots to read.
var dotsBodyTaskRef = regexp.MustCompile(`(?i)\bhk\s+([0-9]+)`)

func registerDotsTools(s *gmcp.Server, service api.Service, p principal) {
	o := operatorTools{service: service, principal: p}
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_inbox", Description: "List open dots requests oldest first, with whether each is started and when. Each request lists its attachments (original_name, size_bytes, mime, sha256, created_at) and, in referenced_attachments, the attachments of up to five tasks its body names as hk <id>, each with from_task; attachments lists files on the request; read them before asking for a copy. Each list holds at most 20 items and sets attachments_truncated or referenced_attachments_truncated when more exist. attachment_get_url is the way to download one. A request whose options list is not empty declares that it ends in the operator's choice: each option has a key, a label and the pictures or videos that show it, and its RESULT opens that decision and must mark one recommended. The run token is never listed. Pass the last task id as cursor to continue.", Annotations: readOnlyAnnotations()}, o.dotsInbox)
	gmcp.AddTool(s, &gmcp.Tool{Name: "dots_reply", Description: "Reply to a dots request. START requires expected_updated_at from the inbox and returns run_token; every later reply must send that run_token. START with takeover true takes over an already started request and rotates the run_token. Use ask_operator for questions requiring an operator decision. When the operator answers a dots question in the chat, record it with decision_answer (the operator's words as text) before continuing; never answer it yourself. On RESULT of a request that declares options, send recommended (one of its option keys), recommendation_reason, unverified and one_fix: hk opens the operator's decision with the declared options, their materials and those fields, and the task completes when the operator answers (decision_answer accepts that answer too). The four fields apply to RESULT only, and recommended must name a declared key. Reuse an idempotency key only for the same reply.", Annotations: writeAnnotations()}, o.dotsReply)
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

// dotsChoiceOptions is the declaration a request carries, or nil: the inbox
// shows it so dots can answer in the shape the operator will choose from.
func dotsChoiceOptions(task store.Task) []store.DecisionOption {
	if task.Refs.DotsChoice == nil {
		return nil
	}
	return task.Refs.DotsChoice.Options
}

type dotsInboxInput struct {
	Cursor int64 `json:"cursor,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

// referencedAttachmentView is one attachment on a task the request body names.
type referencedAttachmentView struct {
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	MIME         string    `json:"mime"`
	OriginalName string    `json:"original_name"`
	CreatedAt    time.Time `json:"created_at"`
	FromTask     int64     `json:"from_task"`
}

// dotsNamedTaskIDs returns up to max task ids the body names as "hk <id>", in
// body order, distinct, and never the request itself.
func dotsNamedTaskIDs(body string, self int64, max int) []int64 {
	seen := map[int64]bool{self: true}
	ids := make([]int64, 0, max)
	for _, match := range dotsBodyTaskRef.FindAllStringSubmatch(body, -1) {
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || id < 1 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) == max {
			break
		}
	}
	return ids
}

func dotsAttachmentView(a store.Attachment) attachmentView {
	return attachmentView{SHA256: a.SHA256, SizeBytes: a.SizeBytes, MIME: a.MIME, OriginalName: a.OriginalName, CreatedAt: a.CreatedAt}
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
		// Started is true once a START claimed the request. RunStartedAt is
		// when its current run token was issued; a takeover moves it.
		Started      bool       `json:"started"`
		RunStartedAt *time.Time `json:"run_started_at,omitempty"`
		// Attachments are the files on the request task itself.
		Attachments          []attachmentView `json:"attachments"`
		AttachmentsTruncated bool             `json:"attachments_truncated"`
		// ReferencedAttachments are files on tasks the body names as "hk <id>".
		ReferencedAttachments          []referencedAttachmentView `json:"referenced_attachments"`
		ReferencedAttachmentsTruncated bool                       `json:"referenced_attachments_truncated"`
		// Options is the choice the request declared it ends in (hk 4789).
		// Non-empty means the request declares that the operator decides:
		// each option has a key, a label and the pictures or videos that show
		// it, and the RESULT marks one recommended and carries its reason.
		Options []store.DecisionOption `json:"options,omitempty"`
	}
	items := make([]entry, 0, len(tasks))
	named := make([][]int64, len(tasks))
	refs := make([]int64, 0, len(tasks)*(dotsInboxNamedTaskLimit+1))
	seen := map[int64]bool{}
	for i, task := range tasks {
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
		runStartedAt, err := o.service.Store.DotsRunIssuedAt(ctx, task.ID)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, entry{Task: task, Body: doc.Body, RequesterLane: task.ParentLane, Replies: comments, Started: task.ClaimedBy != "", RunStartedAt: runStartedAt, Options: dotsChoiceOptions(task)})
		named[i] = dotsNamedTaskIDs(doc.Body, task.ID, dotsInboxNamedTaskLimit)
		for _, id := range append([]int64{task.ID}, named[i]...) {
			if !seen[id] {
				seen[id] = true
				refs = append(refs, id)
			}
		}
	}
	// One read for the whole page: every request task and the tasks its body
	// names, never one query per entry.
	rows, err := o.service.Store.ListTaskAttachmentsByRefs(ctx, refs, dotsInboxAttachmentLimit+1)
	if err != nil {
		return nil, nil, err
	}
	for i := range items {
		ownID := strconv.FormatInt(tasks[i].ID, 10)
		from := make(map[string]int64, len(named[i]))
		for _, id := range named[i] {
			from[strconv.FormatInt(id, 10)] = id
		}
		own := make([]attachmentView, 0)
		referenced := make([]referencedAttachmentView, 0)
		for _, a := range rows {
			if a.RefID == ownID {
				own = append(own, dotsAttachmentView(a))
			} else if id, ok := from[a.RefID]; ok {
				referenced = append(referenced, referencedAttachmentView{SHA256: a.SHA256, SizeBytes: a.SizeBytes, MIME: a.MIME, OriginalName: a.OriginalName, CreatedAt: a.CreatedAt, FromTask: id})
			}
		}
		items[i].AttachmentsTruncated = len(own) > dotsInboxAttachmentLimit
		items[i].Attachments = own[:min(len(own), dotsInboxAttachmentLimit)]
		items[i].ReferencedAttachmentsTruncated = len(referenced) > dotsInboxAttachmentLimit
		items[i].ReferencedAttachments = referenced[:min(len(referenced), dotsInboxAttachmentLimit)]
	}
	var next int64
	if len(tasks) == limit {
		next = tasks[len(tasks)-1].ID
	}
	return jsonResult(map[string]any{"requests": items, "next_cursor": next})
}

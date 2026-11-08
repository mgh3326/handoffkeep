package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP write contract for dots (hk 3465). Every write records an audit row
// with the actor, the channel and the author label; a ChatGPT-channel write is
// labelled dots, so it reads differently from the operator's own writes. A
// keyed write replays its first result when the same key comes back.

const (
	toolTaskUpdate  = "task_update"
	toolDecisionAsk = "decision_ask"
	toolTaskComment = "task_comment"
	toolTaskIntake  = "task_intake"
)

// writeAuthor is the author label a write records. The ChatGPT channel is dots,
// so its writes show as dots; every other write keeps the principal's name.
// The actor (operator:<email>) stays in the audit row.
func writeAuthor(p principal) string {
	if p.Channel == ChannelChatGPT {
		return store.DotsLabel
	}
	return p.Actor
}

// write names one write of this principal for the audit row. key may be empty
// for a write that takes no idempotency key. A key the audit row cannot hold is
// refused here, before the tool writes anything.
func (o operatorTools) write(tool, key string) (store.MCPWrite, error) {
	if err := store.CheckIdempotencyKey(key); err != nil {
		return store.MCPWrite{}, err
	}
	return store.MCPWrite{Tool: tool, Actor: o.principal.Actor, Channel: o.principal.Channel, Author: writeAuthor(o.principal), Key: key}, nil
}

// intakeSourceRef keys an intake by the caller's idempotency key. It is scoped
// to the actor so one caller's key cannot return another caller's task.
func intakeSourceRef(actor, key string) string {
	sum := sha256.Sum256([]byte(actor + "\x00" + key))
	return "chatgpt-key-" + hex.EncodeToString(sum[:])
}

type taskUpdateInput struct {
	ID                int64    `json:"id"`
	ExpectedUpdatedAt string   `json:"expected_updated_at"`
	IdempotencyKey    string   `json:"idempotency_key"`
	State             string   `json:"state,omitempty"`
	Lane              string   `json:"lane,omitempty"`
	Priority          *int     `json:"priority,omitempty"`
	AddLabels         []string `json:"add_labels,omitempty"`
	RemoveLabels      []string `json:"remove_labels,omitempty"`
	Note              string   `json:"note,omitempty"`
}

// taskUpdate changes a task's state, lane or priority. expected_updated_at is
// the updated_at the caller last read (task_show or task_get); a task that
// moved since answers revision_conflict and nothing is written. The same
// idempotency key returns the first call's result.
func (o operatorTools) taskUpdate(ctx context.Context, _ *gmcp.CallToolRequest, in taskUpdateInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	expected, err := parseRFC3339("expected_updated_at", strings.TrimSpace(in.ExpectedUpdatedAt))
	if err != nil {
		return nil, nil, err
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return nil, nil, errors.New("idempotency_key is required")
	}
	write, err := o.write(toolTaskUpdate, key)
	if err != nil {
		return nil, nil, err
	}
	result, err := o.service.Store.UpdateTask(ctx, in.ID, store.TaskUpdateInput{
		ExpectedUpdatedAt: expected, State: strings.TrimSpace(in.State), Lane: strings.TrimSpace(in.Lane),
		Priority: in.Priority, AddLabels: trimNames(in.AddLabels), RemoveLabels: trimNames(in.RemoveLabels),
		Note: in.Note,
	}, write)
	if err != nil {
		return nil, nil, err
	}
	mirrors, err := o.service.Store.ListYouTrackMirrors(ctx, []int64{in.ID})
	if err != nil {
		return nil, nil, err
	}
	mirror, mirrored := mirrors[in.ID]
	out := map[string]any{
		"task":        taskSummaryOf(result.Task, mirror, mirrored),
		"changed":     result.Changed,
		"revision":    result.Revision,
		"replayed":    result.Replayed,
		"console_url": taskURL(in.ID),
		"actor":       o.principal.Actor,
		"channel":     o.principal.Channel,
		"author":      writeAuthor(o.principal),
		// The hk write is committed. sync_pending means the YouTrack copy is
		// queued, not confirmed.
		"youtrack_sync": youTrackSyncStatus(mirrored, len(result.Changed)),
	}
	return jsonResult(out)
}

// trimNames trims each label name the caller sent, so " dots " and "dots" name
// one label.
func trimNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

// youTrackSyncStatus names the YouTrack state of a write: applied when there is
// nothing to push (no mirror, or no change), sync_pending when a mirrored task
// changed and the outbox has the push. sync_failed is not reported here yet:
// this path does not read the outbox's per-task outcome.
func youTrackSyncStatus(mirrored bool, changed int) string {
	if !mirrored || changed == 0 {
		return "applied"
	}
	return "sync_pending"
}

type decisionAskInput struct {
	TaskID         int64                  `json:"task_id"`
	IdempotencyKey string                 `json:"idempotency_key"`
	Question       string                 `json:"question"`
	Options        []store.DecisionOption `json:"options"`
	NoFreeAnswer   bool                   `json:"no_free_answer,omitempty"`
	Reason         string                 `json:"reason,omitempty"`
	DefaultAction  string                 `json:"default_action"`
	DefaultOption  string                 `json:"default_option,omitempty"`
	DefaultTrigger string                 `json:"default_trigger,omitempty"`
	DueAt          string                 `json:"due_at,omitempty"`
	Doc            string                 `json:"doc,omitempty"`
	Refs           []string               `json:"refs,omitempty"`
	Supersedes     string                 `json:"supersedes,omitempty"`
	Block          bool                   `json:"block,omitempty"`
	HumanOnly      bool                   `json:"human_only,omitempty"`
	AnswerPolicy   string                 `json:"answer_policy,omitempty"`
}

// decisionAsk records a structured decision request on a task, through the same
// store path as `tasks decision-request`. Korean display comes from the decision
// templates when the request is read, so nothing is translated here. A request
// id is the store's own (dr-<task>-<revision>). A repeat of the same idempotency
// key returns the first call's request with replayed set; the request and its
// audit row commit together.
func (o operatorTools) decisionAsk(ctx context.Context, _ *gmcp.CallToolRequest, in decisionAskInput) (*gmcp.CallToolResult, any, error) {
	if in.TaskID < 1 {
		return nil, nil, errors.New("task_id must be positive")
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return nil, nil, errors.New("idempotency_key is required")
	}
	write, err := o.write(toolDecisionAsk, key)
	if err != nil {
		return nil, nil, err
	}
	input := store.DecisionRequestInput{
		Question: strings.TrimSpace(in.Question), Reason: strings.TrimSpace(in.Reason),
		DefaultAction: strings.TrimSpace(in.DefaultAction), DefaultOption: strings.TrimSpace(in.DefaultOption),
		DefaultTrigger: strings.TrimSpace(in.DefaultTrigger), Doc: strings.TrimSpace(in.Doc),
		Refs: in.Refs, Supersedes: strings.TrimSpace(in.Supersedes), Block: in.Block,
		HumanOnly: in.HumanOnly, AnswerPolicy: strings.TrimSpace(in.AnswerPolicy),
		Options: store.DecisionOptions{Options: in.Options, AllowFree: !in.NoFreeAnswer},
	}
	if strings.TrimSpace(in.DueAt) != "" {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(in.DueAt))
		if err != nil {
			return nil, nil, errors.New("due_at must be RFC3339 with a timezone offset")
		}
		input.DueAt = &at
	}
	recorded, replayed, err := o.service.Store.RecordMCPDecisionRequest(ctx, in.TaskID, input, write)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{
		"request_id":  recorded.Request.ID,
		"request":     recorded.Request,
		"duplicate":   recorded.Duplicate,
		"replayed":    replayed,
		"task":        map[string]any{"id": recorded.Task.ID, "state": recorded.Task.State, "console_url": taskURL(recorded.Task.ID)},
		"console_url": taskURL(recorded.Task.ID),
		"actor":       o.principal.Actor,
		"channel":     o.principal.Channel,
		"author":      write.Author,
	})
}

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	taskGetPreviewLimit      = 5
	taskCommentsDefaultLimit = 50
	taskCommentsMaxLimit     = 100
	taskEventsDefaultLimit   = 50
	taskEventsMaxLimit       = 100
	changesSinceDefaultLimit = 50
	changesSinceMaxLimit     = 100
	// maxOpaqueCursorBytes bounds a cursor before it is decoded. A cursor is
	// one small JSON object in base64url; anything larger is a client error.
	maxOpaqueCursorBytes = 2048
)

// taskFieldSources names the canonical system for each task field (hk:doc
// design/2026-11-07/youtrack-first-operator-ui, hk 3461). "hk" is the hk
// Postgres record the agents write; "youtrack" is the operator's system of
// record. It is served with task_get so a reader never has to guess which
// side owns a value it is about to act on.
var taskFieldSources = map[string]string{
	"state":               "hk",
	"priority":            "hk",
	"lane":                "hk",
	"kind":                "hk",
	"project":             "hk",
	"area":                "hk",
	"labels":              "hk",
	"claimed_by":          "hk",
	"refs":                "hk",
	"comments_by_agents":  "hk",
	"operator_comments":   "youtrack",
	"korean_display_text": "youtrack",
}

// cursorKind tags an opaque cursor so a cursor from one tool cannot be
// replayed against another.
type cursorKind string

const (
	cursorTaskList   cursorKind = "task_list"
	cursorComments   cursorKind = "task_comments"
	cursorTaskEvents cursorKind = "task_events"
	cursorChanges    cursorKind = "changes_since"
)

var errInvalidCursor = errors.New("invalid cursor")

// mcpCursor is the decoded form of every cursor these tools hand out. Time
// carries the (updated_at,id) keyset position for task_list; ID carries the
// after-id position for comments and events; Sync carries the sync_log
// position for changes_since.
type mcpCursor struct {
	Kind string `json:"k"`
	Time string `json:"t,omitempty"`
	ID   int64  `json:"i,omitempty"`
	Sync int64  `json:"s,omitempty"`
}

func encodeCursor(c mcpCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw string, kind cursorKind, c *mcpCursor) error {
	if raw == "" || len(raw) > maxOpaqueCursorBytes {
		return errInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) > maxOpaqueCursorBytes {
		return errInvalidCursor
	}
	if err := json.Unmarshal(b, c); err != nil || c.Kind != string(kind) {
		return errInvalidCursor
	}
	return nil
}

func parseRFC3339(name, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	return t.UTC(), nil
}

func taskEventViews(events []store.TaskEvent) []taskEventView {
	out := make([]taskEventView, 0, len(events))
	for _, event := range events {
		out = append(out, taskEventView{
			ID: event.ID, Kind: event.Kind, From: event.From, To: event.To, By: event.By,
			Note: event.Note, Field: event.Field, FromValue: event.FromValue, ToValue: event.ToValue,
			Source: event.Source, At: event.At,
		})
	}
	return out
}

type translationView struct {
	Text  string `json:"text"`
	Stale bool   `json:"stale"`
}

type blockerView struct {
	RelationID int64            `json:"relation_id"`
	Task       *store.TaskChild `json:"task,omitempty"`
}

type taskGetInput struct {
	ID int64 `json:"id"`
}

// taskGet is the complete single-task read: everything a reader needs to act
// on a task without a second round trip, except unbounded comment and event
// threads, which have their own paging tools.
func (o operatorTools) taskGet(ctx context.Context, _ *gmcp.CallToolRequest, in taskGetInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	task, found, err := o.service.GetTask(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, errors.New("not_found")
	}
	body := ""
	bodyKey := task.BodyDoc
	if key, _, hasSection := strings.Cut(task.BodyDoc, "#"); hasSection {
		bodyKey = key
	}
	if bodyKey != "" {
		doc, ok, err := o.service.GetDocument(ctx, bodyKey)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			body = doc.Body
		}
	}
	mirrors, err := o.service.Store.ListYouTrackMirrors(ctx, []int64{task.ID})
	if err != nil {
		return nil, nil, err
	}
	mirror, mirrored := mirrors[task.ID]
	summary := taskSummaryOf(task, mirror, mirrored)

	var owner *store.ProjectOwner
	if task.Project != nil && *task.Project != "" {
		projectOwner, err := o.service.Store.GetProjectOwner(ctx, *task.Project)
		if err != nil && !errors.Is(err, store.ErrProjectNotFound) {
			return nil, nil, err
		}
		if err == nil {
			owner = &projectOwner
		}
	}

	translations := map[string]translationView{}
	sources := []store.TranslationSource{{
		EntityID: strconv.FormatInt(task.ID, 10), Field: "title", Original: task.Title,
	}}
	if body != "" {
		sources = append(sources, store.TranslationSource{
			EntityID: strconv.FormatInt(task.ID, 10), Field: "body", Original: body,
		})
	}
	translated, err := o.service.Store.ListTranslations(ctx, "task", sources)
	if err != nil {
		return nil, nil, err
	}
	for _, row := range translated {
		translations[row.Field] = translationView{Text: row.Text, Stale: row.Stale}
	}

	briefIDs := []int64{}
	if task.ParentID != nil {
		briefIDs = append(briefIDs, *task.ParentID)
	}
	for _, relation := range task.Relations {
		if relation.Kind == store.RelationBlocks && relation.DstID == task.ID {
			briefIDs = append(briefIDs, relation.SrcID)
		}
	}
	briefs, err := o.service.Store.TaskBriefs(ctx, briefIDs)
	if err != nil {
		return nil, nil, err
	}
	var parent *store.TaskChild
	if task.ParentID != nil {
		child := store.TaskChild{ID: *task.ParentID}
		if brief, ok := briefs[*task.ParentID]; ok {
			child = brief
		}
		parent = &child
	}
	blockers := make([]blockerView, 0)
	for _, relation := range task.Relations {
		if relation.Kind != store.RelationBlocks || relation.DstID != task.ID {
			continue
		}
		blocker := blockerView{RelationID: relation.ID}
		if brief, ok := briefs[relation.SrcID]; ok {
			blocker.Task = &brief
		}
		blockers = append(blockers, blocker)
	}

	comments, err := o.service.Store.RecentTaskComments(ctx, task.ID, taskGetPreviewLimit+1)
	if err != nil {
		return nil, nil, err
	}
	commentsMore := len(comments) > taskGetPreviewLimit
	if commentsMore {
		comments = comments[len(comments)-taskGetPreviewLimit:]
	}
	events := task.Events
	eventsMore := len(events) > taskGetPreviewLimit
	if eventsMore {
		events = events[len(events)-taskGetPreviewLimit:]
	}

	var closedAt *time.Time
	switch {
	case task.CompletedAt != nil:
		closedAt = task.CompletedAt
	case task.CanceledAt != nil:
		closedAt = task.CanceledAt
	}

	return jsonResult(map[string]any{
		"id":               summary.ID,
		"title":            summary.Title,
		"body":             body,
		"body_doc":         task.BodyDoc,
		"state":            summary.State,
		"priority":         summary.Priority,
		"kind":             task.Kind,
		"lane":             summary.Lane,
		"project":          summary.Project,
		"area":             task.Area,
		"labels":           task.Labels,
		"claimed_by":       task.ClaimedBy,
		"created_by":       task.CreatedBy,
		"refs":             task.Refs,
		"created_at":       task.CreatedAt,
		"updated_at":       summary.UpdatedAt,
		"started_at":       task.StartedAt,
		"completed_at":     task.CompletedAt,
		"canceled_at":      task.CanceledAt,
		"closed_at":        closedAt,
		"parent":           parent,
		"children":         task.Children,
		"relations":        task.Relations,
		"blockers":         blockers,
		"project_owner":    owner,
		"translations":     translations,
		"comments_preview": comments,
		"comments_more":    commentsMore,
		"events_preview":   taskEventViews(events),
		"events_more":      eventsMore,
		"youtrack":         summary.YouTrack,
		"console_url":      summary.ConsoleURL,
		"field_sources":    taskFieldSources,
	})
}

type taskCommentsInput struct {
	ID     int64   `json:"id"`
	Cursor *string `json:"cursor,omitempty"`
	Limit  int     `json:"limit,omitempty"`
}

// taskComments pages a task's comments oldest to newest. The cursor is opaque;
// an empty cursor starts at the oldest comment.
func (o operatorTools) taskComments(ctx context.Context, _ *gmcp.CallToolRequest, in taskCommentsInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	limit := clampLimit(in.Limit, taskCommentsDefaultLimit, taskCommentsMaxLimit)
	var after int64
	if in.Cursor != nil && *in.Cursor != "" {
		var cur mcpCursor
		if err := decodeCursor(*in.Cursor, cursorComments, &cur); err != nil {
			return nil, nil, err
		}
		if cur.ID < 0 {
			return nil, nil, errInvalidCursor
		}
		after = cur.ID
	}
	rows, err := o.service.ListTaskComments(ctx, in.ID, after, limit+1)
	if errors.Is(err, store.ErrTaskNotFound) {
		return nil, nil, errors.New("not_found")
	}
	if err != nil {
		return nil, nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	next := ""
	if hasMore && len(rows) > 0 {
		next = encodeCursor(mcpCursor{Kind: string(cursorComments), ID: rows[len(rows)-1].ID})
	}
	return jsonResult(map[string]any{"comments": rows, "next_cursor": next, "has_more": hasMore})
}

type taskEventsInput struct {
	ID     int64   `json:"id"`
	Cursor *string `json:"cursor,omitempty"`
	Limit  int     `json:"limit,omitempty"`
}

// taskEvents pages a task's events oldest to newest. An event id is issued in
// append order, so an after-id cursor never repeats or skips a row.
func (o operatorTools) taskEvents(ctx context.Context, _ *gmcp.CallToolRequest, in taskEventsInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	limit := clampLimit(in.Limit, taskEventsDefaultLimit, taskEventsMaxLimit)
	var after int64
	if in.Cursor != nil && *in.Cursor != "" {
		var cur mcpCursor
		if err := decodeCursor(*in.Cursor, cursorTaskEvents, &cur); err != nil {
			return nil, nil, err
		}
		if cur.ID < 0 {
			return nil, nil, errInvalidCursor
		}
		after = cur.ID
	}
	rows, err := o.service.Store.ListTaskEvents(ctx, in.ID, after, limit+1)
	if errors.Is(err, store.ErrTaskNotFound) {
		return nil, nil, errors.New("not_found")
	}
	if err != nil {
		return nil, nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	next := ""
	if hasMore && len(rows) > 0 {
		next = encodeCursor(mcpCursor{Kind: string(cursorTaskEvents), ID: rows[len(rows)-1].ID})
	}
	return jsonResult(map[string]any{"events": rows, "next_cursor": next, "has_more": hasMore})
}

type changeItem struct {
	SyncID  int64        `json:"sync_id"`
	Cursor  string       `json:"cursor"`
	Kind    string       `json:"kind"`
	Model   string       `json:"model"`
	ModelID string       `json:"model_id"`
	Action  string       `json:"action"`
	TaskID  *int64       `json:"task_id,omitempty"`
	Task    *taskSummary `json:"task,omitempty"`
	At      time.Time    `json:"at"`
}

type changesSinceInput struct {
	Cursor *string `json:"cursor,omitempty"`
	Since  string  `json:"since,omitempty"`
	Limit  int     `json:"limit,omitempty"`
}

// changeKind names a feed row for a watcher. A task row is created, updated
// (a transition is an update whose task state changed) or deleted; comments
// and the other synced models keep their own names.
func changeKind(model, action string) string {
	switch model {
	case "task":
		switch action {
		case "I":
			return "task_created"
		case "D":
			return "task_deleted"
		}
		return "task_updated"
	case "task_comment":
		return "task_commented"
	case "decision":
		return "decision_changed"
	case "document":
		return "document_changed"
	}
	return model + "_changed"
}

func changeTaskID(model, modelID string) int64 {
	id, err := strconv.ParseInt(modelID, 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	if model != "task" {
		return 0
	}
	return id
}

// changesSince serves the sync_log feed as one stream a watcher can process
// exactly once: each row has a monotonic sync_id, and next_cursor is the last
// row this call returned. The feed is the same one the CLI reads with
// `handoffkeep sync delta`; see docs/mcp.md for its retention.
func (o operatorTools) changesSince(ctx context.Context, _ *gmcp.CallToolRequest, in changesSinceInput) (*gmcp.CallToolResult, any, error) {
	limit := clampLimit(in.Limit, changesSinceDefaultLimit, changesSinceMaxLimit)
	if in.Cursor != nil && *in.Cursor != "" && strings.TrimSpace(in.Since) != "" {
		return nil, nil, errors.New("pass cursor or since, not both")
	}
	var after int64
	switch {
	case in.Cursor != nil && *in.Cursor != "":
		var cur mcpCursor
		if err := decodeCursor(*in.Cursor, cursorChanges, &cur); err != nil {
			return nil, nil, err
		}
		if cur.Sync < 0 {
			return nil, nil, errInvalidCursor
		}
		after = cur.Sync
	case strings.TrimSpace(in.Since) != "":
		since, err := parseRFC3339("since", strings.TrimSpace(in.Since))
		if err != nil {
			return nil, nil, err
		}
		if after, err = o.service.Store.SyncIDAt(ctx, since); err != nil {
			return nil, nil, err
		}
	}
	changes, meta, err := o.service.Store.SyncDelta(ctx, after, limit+1)
	if err != nil {
		var reset *store.SyncReset
		if errors.As(err, &reset) {
			minCursor := encodeCursor(mcpCursor{Kind: string(cursorChanges), Sync: reset.MinSyncID})
			return jsonResult(map[string]any{
				"items":       []changeItem{},
				"next_cursor": minCursor,
				"min_cursor":  minCursor,
				"max_cursor":  encodeCursor(mcpCursor{Kind: string(cursorChanges), Sync: meta.MaxSyncID}),
				"has_more":    false,
				"reset":       true,
				"message":     "the cursor predates the server's sync retention; continue from min_cursor and expect a gap",
			})
		}
		if errors.Is(err, store.ErrInvalidSync) {
			return nil, nil, errInvalidCursor
		}
		return nil, nil, err
	}
	hasMore := len(changes) > limit
	if hasMore {
		changes = changes[:limit]
	}
	nextSync := meta.LastSyncID
	if hasMore && len(changes) > 0 {
		nextSync = changes[len(changes)-1].SyncID
	}

	taskIDs := map[int64]bool{}
	commentIDs := []int64{}
	for _, change := range changes {
		switch change.Model {
		case "task":
			if id := changeTaskID(change.Model, change.ModelID); id > 0 {
				taskIDs[id] = true
			}
		case "task_comment":
			if change.Action == "D" {
				continue
			}
			if id, err := strconv.ParseInt(change.ModelID, 10, 64); err == nil && id > 0 {
				commentIDs = append(commentIDs, id)
			}
		}
	}
	commentTasks, err := o.service.Store.TaskIDsForComments(ctx, commentIDs)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range commentTasks {
		taskIDs[id] = true
	}
	ids := make([]int64, 0, len(taskIDs))
	for id := range taskIDs {
		ids = append(ids, id)
	}
	tasks, err := o.service.Store.TasksByIDs(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	summaries, err := o.summaries(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]taskSummary, len(summaries))
	for _, summary := range summaries {
		byID[summary.ID] = summary
	}

	items := make([]changeItem, 0, len(changes))
	for _, change := range changes {
		item := changeItem{
			SyncID: change.SyncID, Cursor: encodeCursor(mcpCursor{Kind: string(cursorChanges), Sync: change.SyncID}),
			Kind: changeKind(change.Model, change.Action), Model: change.Model, ModelID: change.ModelID,
			Action: change.Action, At: change.At,
		}
		taskID := changeTaskID(change.Model, change.ModelID)
		if taskID == 0 && change.Model == "task_comment" {
			if id, err := strconv.ParseInt(change.ModelID, 10, 64); err == nil {
				taskID = commentTasks[id]
			}
		}
		if taskID > 0 {
			id := taskID
			item.TaskID = &id
			if summary, ok := byID[taskID]; ok {
				item.Task = &summary
			}
		}
		items = append(items, item)
	}
	return jsonResult(map[string]any{
		"items":       items,
		"next_cursor": encodeCursor(mcpCursor{Kind: string(cursorChanges), Sync: nextSync}),
		"max_cursor":  encodeCursor(mcpCursor{Kind: string(cursorChanges), Sync: meta.MaxSyncID}),
		"has_more":    hasMore,
		"reset":       false,
	})
}

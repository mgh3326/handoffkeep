package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The console origin every task result links to. The design session runs the
// console at this hostname; the MCP host has no separate base-URL setting.
const consoleBaseURL = "https://console.robinco.dev"

const (
	taskSearchDefaultLimit   = 20
	taskSearchMaxLimit       = 50
	taskListDefaultLimit     = 20
	taskListMaxLimit         = 100
	taskShowCommentLimit     = 20
	taskShowEventLimit       = 10
	decisionsPendingLimit    = 100
	duplicateCandidateLimit  = 5
	duplicateCandidateWindow = 10
)

type operatorTools struct {
	service   api.Service
	principal principal
}

// registerOperatorTools adds the triage and decision tools to an HTTP MCP
// server. Both credential kinds reach them; the principal fixes attribution.
func registerOperatorTools(s *gmcp.Server, service api.Service, p principal) {
	o := operatorTools{service: service, principal: p}
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_search",
		Description: "Search hk tasks by text, using the console search path including Korean translations. Optional state, lane and project filters narrow the result. Returns JSON task summaries with console_url and the YouTrack id when the task is mirrored.",
		Annotations: readOnlyAnnotations(),
	}, o.taskSearch)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_show",
		Description: "Show one hk task: title with its Korean translation when present, state, priority, lane, project, refs.pr, the last 20 comments and the last 10 events. Returns JSON with console_url and the YouTrack id when the task is mirrored.",
		Annotations: readOnlyAnnotations(),
	}, o.taskShow)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_get",
		Description: "Read one hk task in full: body text (from its hk document), state, priority, kind, lane, project, area, labels, claimed_by, refs, parent, children, relations and blockers, the project's design owner, translations, YouTrack link, created_at/updated_at/closed_at and console_url. field_sources names the canonical system for every returned task data field: hk owns the work record (including state, priority, lane, refs and comments by agents); YouTrack owns operator comments, translations/Korean display text and the mirror. Comments and events are a short preview only; page them with task_comments and task_events.",
		Annotations: readOnlyAnnotations(),
	}, o.taskGet)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_list",
		Description: "List hk tasks with optional state, lane and project filters, up to 100. Pass cursor (an empty string for the first page) for deterministic pages ordered by updated_at then id, and follow next_cursor until it is empty; updated_since (RFC3339) starts at tasks updated at or after that time. Without cursor or updated_since this is the previous unpaged list. Returns JSON task summaries with console_url and the YouTrack id when a task is mirrored.",
		Annotations: readOnlyAnnotations(),
	}, o.taskList)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_comments",
		Description: "Page one hk task's comments oldest to newest, up to 100 per page. Pass id and an opaque cursor (empty for the first page) and follow next_cursor until has_more is false.",
		Annotations: readOnlyAnnotations(),
	}, o.taskComments)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_events",
		Description: "Page one hk task's events oldest to newest, up to 100 per page. Pass id and an opaque cursor (empty for the first page) and follow next_cursor until has_more is false.",
		Annotations: readOnlyAnnotations(),
	}, o.taskEvents)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "changes_since",
		Description: "Read the hk change feed across all tasks: task created/updated/deleted, comment, decision and document rows, oldest first, each with a monotonic sync_id and an opaque per-item cursor. A state transition is a task_updated row; task_current is the task's summary when the feed is read, not the state at that historical row, so use task_events for the ordered transition history. Start with no cursor (or an RFC3339 since) and follow next_cursor; each change appears exactly once. max_cursor is the feed's current end. A cursor older than the server's sync retention answers reset:true with min_cursor, the position before the oldest retained row.",
		Annotations: readOnlyAnnotations(),
	}, o.changesSince)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "decisions_pending",
		Description: "List the open structured decision requests awaiting an operator answer, with their question, options and Korean translations when present. Answer one with decision_answer.",
		Annotations: readOnlyAnnotations(),
	}, o.decisionsPending)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "decision_answer",
		Description: "Answer one open hk decision request by request id, with an option key or free text and an optional note. Uses the same store path and validation as the console answer. An already answered or stale request returns the store's error code.",
		Annotations: writeAnnotations(),
	}, o.decisionAnswer)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_comment",
		Description: "Append one comment to an hk task. Comments are data only: they never change state, priority or lane. The author is the authenticated principal.",
		Annotations: writeAnnotations(),
	}, o.taskComment)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_intake",
		Description: "File a new hk task into triage. Returns the created task, or the task already recorded for an identical intake, plus duplicate candidates found before filing when there are any.",
		Annotations: writeAnnotations(),
	}, o.taskIntake)
	registerMetaTools(s, o)
}

func readOnlyAnnotations() *gmcp.ToolAnnotations {
	return &gmcp.ToolAnnotations{ReadOnlyHint: true}
}

func writeAnnotations() *gmcp.ToolAnnotations {
	no := false
	return &gmcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no}
}

func jsonResult(v any) (*gmcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(b)}}}, nil, nil
}

func clampLimit(value, fallback, max int) int {
	if value < 1 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func taskURL(id int64) string {
	return consoleBaseURL + "/ui/tasks/" + strconv.FormatInt(id, 10)
}

type youTrackRef struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`
}

type taskSummary struct {
	ID         int64        `json:"id"`
	Title      string       `json:"title"`
	Snippet    string       `json:"snippet,omitempty"`
	State      string       `json:"state"`
	Priority   int          `json:"priority"`
	Lane       string       `json:"lane"`
	Project    *string      `json:"project"`
	PR         string       `json:"pr,omitempty"`
	UpdatedAt  time.Time    `json:"updated_at"`
	ConsoleURL string       `json:"console_url"`
	YouTrack   *youTrackRef `json:"youtrack,omitempty"`
}

func taskSummaryOf(task store.Task, mirror store.YouTrackMirror, mirrored bool) taskSummary {
	out := taskSummary{
		ID: task.ID, Title: task.Title, State: task.State, Priority: task.Priority,
		Lane: task.Lane, Project: task.Project, PR: task.Refs.PR, UpdatedAt: task.UpdatedAt,
		ConsoleURL: taskURL(task.ID),
	}
	if mirrored && mirror.DisplayRef != "" {
		out.YouTrack = &youTrackRef{ID: mirror.DisplayRef, URL: mirror.URL}
	}
	return out
}

func (o operatorTools) summaries(ctx context.Context, tasks []store.Task) ([]taskSummary, error) {
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	mirrors, err := o.service.Store.ListYouTrackMirrors(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]taskSummary, 0, len(tasks))
	for _, task := range tasks {
		mirror, mirrored := mirrors[task.ID]
		out = append(out, taskSummaryOf(task, mirror, mirrored))
	}
	return out, nil
}

func parseStateFilter(value string) (store.StateFilter, bool) {
	if value == "" {
		return store.StateFilter{}, true
	}
	return store.ParseStateFilter(value)
}

func stateMatches(task store.Task, filter store.StateFilter) bool {
	if filter.Awaiting {
		return task.AwaitingDecision
	}
	if len(filter.States) == 0 {
		return true
	}
	for _, state := range filter.States {
		if task.State == state {
			return true
		}
	}
	return false
}

type taskSearchInput struct {
	Query   string `json:"query"`
	State   string `json:"state,omitempty"`
	Lane    string `json:"lane,omitempty"`
	Project string `json:"project,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (o operatorTools) taskSearch(ctx context.Context, _ *gmcp.CallToolRequest, in taskSearchInput) (*gmcp.CallToolResult, any, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	stateFilter, ok := parseStateFilter(strings.TrimSpace(in.State))
	if !ok {
		return nil, nil, fmt.Errorf("unknown state %q", in.State)
	}
	limit := clampLimit(in.Limit, taskSearchDefaultLimit, taskSearchMaxLimit)
	// Filters run after the store's text search, so a filtered request must
	// read the whole search window before it limits; otherwise hits that do
	// not match a filter hide matching hits ranked just after them.
	searchLimit := limit
	if len(stateFilter.States) > 0 || stateFilter.Awaiting || in.Project != "" {
		searchLimit = taskSearchMaxLimit
	}
	results, err := o.service.Search(ctx, query, "tasks", strings.TrimSpace(in.Lane), searchLimit)
	if err != nil {
		return nil, nil, err
	}
	type hit struct {
		task    store.Task
		snippet string
	}
	hits := make([]hit, 0, len(results))
	searchTruncated := false
	for _, result := range results {
		if result.Truncated {
			searchTruncated = true
		}
		id, err := strconv.ParseInt(result.Key, 10, 64)
		if err != nil {
			continue
		}
		task, found, err := o.service.GetTask(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if !found || !stateMatches(task, stateFilter) {
			continue
		}
		if in.Project != "" && (task.Project == nil || *task.Project != in.Project) {
			continue
		}
		hits = append(hits, hit{task: task, snippet: result.Snippet})
	}
	// A cut search window stays truncated even when the filtered page is
	// full: matches may exist beyond the window, so a full filtered page is
	// not proof that the result is complete.
	truncated := searchTruncated
	if len(hits) > limit {
		hits = hits[:limit]
		truncated = true
	}
	tasks := make([]store.Task, 0, len(hits))
	for _, h := range hits {
		tasks = append(tasks, h.task)
	}
	summaries, err := o.summaries(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	for i := range summaries {
		summaries[i].Snippet = hits[i].snippet
	}
	return jsonResult(map[string]any{"query": query, "results": summaries, "truncated": truncated})
}

type taskShowInput struct {
	ID int64 `json:"id"`
}

type taskEventView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	By        string    `json:"by"`
	Note      string    `json:"note,omitempty"`
	Field     string    `json:"field,omitempty"`
	FromValue string    `json:"from_value,omitempty"`
	ToValue   string    `json:"to_value,omitempty"`
	Source    string    `json:"source,omitempty"`
	At        time.Time `json:"at"`
}

func (o operatorTools) taskShow(ctx context.Context, _ *gmcp.CallToolRequest, in taskShowInput) (*gmcp.CallToolResult, any, error) {
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
	mirrors, err := o.service.Store.ListYouTrackMirrors(ctx, []int64{task.ID})
	if err != nil {
		return nil, nil, err
	}
	mirror, mirrored := mirrors[task.ID]
	summary := taskSummaryOf(task, mirror, mirrored)
	comments, err := o.lastComments(ctx, task.ID, taskShowCommentLimit)
	if err != nil {
		return nil, nil, err
	}
	events := task.Events
	if len(events) > taskShowEventLimit {
		events = events[len(events)-taskShowEventLimit:]
	}
	eventViews := taskEventViews(events)
	titleKO, titleKOStale, err := o.taskTitleKO(ctx, task)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{
		"id":                summary.ID,
		"title":             summary.Title,
		"title_ko":          titleKO,
		"title_ko_stale":    titleKOStale,
		"state":             summary.State,
		"priority":          summary.Priority,
		"lane":              summary.Lane,
		"project":           summary.Project,
		"refs":              map[string]any{"pr": task.Refs.PR},
		"awaiting_decision": task.AwaitingDecision,
		"updated_at":        summary.UpdatedAt,
		"console_url":       summary.ConsoleURL,
		"youtrack":          summary.YouTrack,
		"comments":          comments,
		"events":            eventViews,
	})
}

func (o operatorTools) taskTitleKO(ctx context.Context, task store.Task) (string, bool, error) {
	rows, err := o.service.Store.ListTranslations(ctx, "task", []store.TranslationSource{{
		EntityID: strconv.FormatInt(task.ID, 10),
		Field:    "title",
		Original: task.Title,
	}})
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].Text, rows[0].Stale, nil
}

// lastComments reads the newest comments. The store pages forward only, so a
// full page is followed until a short page arrives; then the tail is taken.
func (o operatorTools) lastComments(ctx context.Context, taskID int64, limit int) ([]store.TaskComment, error) {
	all := []store.TaskComment{}
	var after int64
	for page := 0; page < 100; page++ {
		rows, err := o.service.ListTaskComments(ctx, taskID, after, store.TaskCommentListMax)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		all = append(all, rows...)
		after = rows[len(rows)-1].ID
		if len(rows) < store.TaskCommentListMax {
			break
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

type taskListInput struct {
	State        string  `json:"state,omitempty"`
	Lane         string  `json:"lane,omitempty"`
	Project      string  `json:"project,omitempty"`
	Limit        int     `json:"limit,omitempty"`
	Cursor       *string `json:"cursor,omitempty"`
	UpdatedSince string  `json:"updated_since,omitempty"`
}

func (o operatorTools) taskList(ctx context.Context, _ *gmcp.CallToolRequest, in taskListInput) (*gmcp.CallToolResult, any, error) {
	// A request naming neither cursor nor updated_since is the previous
	// unpaged list; its output stays exactly what it was, so an existing
	// client sees no change.
	if in.Cursor == nil && strings.TrimSpace(in.UpdatedSince) == "" {
		return o.taskListUnpaged(ctx, in)
	}
	return o.taskListPaged(ctx, in)
}

func (o operatorTools) taskListUnpaged(ctx context.Context, in taskListInput) (*gmcp.CallToolResult, any, error) {
	limit := clampLimit(in.Limit, taskListDefaultLimit, taskListMaxLimit)
	query := store.TaskQuery{Lane: strings.TrimSpace(in.Lane), State: strings.TrimSpace(in.State), Limit: limit + 1}
	if in.Project != "" {
		project := in.Project
		query.Project = &project
	}
	tasks, err := o.service.ListTasksFiltered(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	truncated := len(tasks) > limit
	if truncated {
		tasks = tasks[:limit]
	}
	summaries, err := o.summaries(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"tasks": summaries, "truncated": truncated})
}

// taskListPaged orders by (updated_at,id), oldest first, and pages with an
// opaque keyset cursor. A task updated between two pages moves ahead of the
// cursor; the changes_since feed is the exactly-once view for a watcher.
func (o operatorTools) taskListPaged(ctx context.Context, in taskListInput) (*gmcp.CallToolResult, any, error) {
	limit := clampLimit(in.Limit, taskListDefaultLimit, taskListMaxLimit)
	query := store.TaskQuery{Lane: strings.TrimSpace(in.Lane), State: strings.TrimSpace(in.State), Limit: limit + 1}
	if in.Project != "" {
		project := in.Project
		query.Project = &project
	}
	if value := strings.TrimSpace(in.UpdatedSince); value != "" {
		since, err := parseRFC3339("updated_since", value)
		if err != nil {
			return nil, nil, err
		}
		query.UpdatedSince = &since
	}
	if in.Cursor != nil && *in.Cursor != "" {
		var cur mcpCursor
		if err := decodeCursor(*in.Cursor, cursorTaskList, &cur); err != nil {
			return nil, nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, cur.Time)
		if err != nil || cur.ID < 0 {
			return nil, nil, errInvalidCursor
		}
		query.AfterUpdatedAt = &at
		afterID := cur.ID
		query.AfterID = &afterID
	}
	tasks, err := o.service.ListTasksFiltered(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	hasMore := len(tasks) > limit
	if hasMore {
		tasks = tasks[:limit]
	}
	next := ""
	if hasMore && len(tasks) > 0 {
		last := tasks[len(tasks)-1]
		next = encodeCursor(mcpCursor{Kind: string(cursorTaskList), Time: last.UpdatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
	}
	summaries, err := o.summaries(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"tasks": summaries, "truncated": hasMore, "next_cursor": next})
}

type decisionOptionView struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended,omitempty"`
}

type decisionView struct {
	RequestID       string               `json:"request_id"`
	TaskID          int64                `json:"task_id"`
	TaskTitle       string               `json:"task_title"`
	TaskTitleKO     string               `json:"task_title_ko,omitempty"`
	Question        string               `json:"question"`
	QuestionKO      string               `json:"question_ko,omitempty"`
	QuestionKOStale bool                 `json:"question_ko_stale,omitempty"`
	Options         []decisionOptionView `json:"options"`
	OptionsKO       []decisionOptionView `json:"options_ko,omitempty"`
	OptionsKOStale  bool                 `json:"options_ko_stale,omitempty"`
	AllowFree       bool                 `json:"allow_free"`
	State           string               `json:"state"`
	DefaultAction   string               `json:"default_action"`
	RequestedBy     string               `json:"requested_by"`
	RequestedAt     time.Time            `json:"requested_at"`
	DueAt           *time.Time           `json:"due_at,omitempty"`
	ConsoleURL      string               `json:"console_url"`
}

func decisionOptionViews(options []store.DecisionOption) []decisionOptionView {
	out := make([]decisionOptionView, 0, len(options))
	for _, option := range options {
		out = append(out, decisionOptionView{Key: option.Key, Label: option.Label, Recommended: option.Recommended})
	}
	return out
}

func (o operatorTools) decisionsPending(ctx context.Context, _ *gmcp.CallToolRequest, _ struct{}) (*gmcp.CallToolResult, any, error) {
	tasks, err := o.service.Store.ListOpenDecisionRequests(ctx, decisionsPendingLimit+1)
	if err != nil {
		return nil, nil, err
	}
	truncated := len(tasks) > decisionsPendingLimit
	if truncated {
		tasks = tasks[:decisionsPendingLimit]
	}
	now := time.Now().UTC()
	items := make([]decisionView, 0, len(tasks))
	sources := make([]store.TranslationSource, 0, len(tasks)*2)
	taskSources := make([]store.TranslationSource, 0, len(tasks))
	for _, task := range tasks {
		request := task.Refs.DecisionRequest
		if request == nil || request.Status != store.DecisionRequestOpen {
			continue
		}
		item := decisionView{
			RequestID: request.ID, TaskID: task.ID, TaskTitle: task.Title,
			Question: request.Question, Options: []decisionOptionView{}, State: store.DecisionRequestState(*request, task.State, false, now),
			DefaultAction: request.DefaultAction, RequestedBy: request.RequestedBy, RequestedAt: request.RequestedAt,
			DueAt: request.DueAt, ConsoleURL: taskURL(task.ID),
		}
		if task.Refs.DecisionOptions != nil {
			item.Options = decisionOptionViews(task.Refs.DecisionOptions.Options)
			item.AllowFree = task.Refs.DecisionOptions.AllowFree
		}
		taskSources = append(taskSources, store.TranslationSource{
			EntityID: strconv.FormatInt(task.ID, 10), Field: "title", Original: task.Title,
		})
		sources = append(sources, store.TranslationSource{EntityID: request.ID, Field: "question", Original: request.Question})
		if len(item.Options) > 0 {
			raw, err := json.Marshal(store.DecisionOptions{Options: task.Refs.DecisionOptions.Options, AllowFree: task.Refs.DecisionOptions.AllowFree})
			if err != nil {
				return nil, nil, err
			}
			sources = append(sources, store.TranslationSource{EntityID: request.ID, Field: "options", Original: string(raw)})
		}
		items = append(items, item)
	}
	taskTranslations, err := o.service.Store.ListTranslations(ctx, "task", taskSources)
	if err != nil {
		return nil, nil, err
	}
	titleKO := map[int64]string{}
	for _, translation := range taskTranslations {
		id, err := strconv.ParseInt(translation.EntityID, 10, 64)
		if err == nil {
			titleKO[id] = translation.Text
		}
	}
	for i := range items {
		items[i].TaskTitleKO = titleKO[items[i].TaskID]
	}
	translations, err := o.service.Store.ListTranslations(ctx, "decision_request", sources)
	if err != nil {
		return nil, nil, err
	}
	byEntity := map[string]map[string]store.Translation{}
	for _, translation := range translations {
		if byEntity[translation.EntityID] == nil {
			byEntity[translation.EntityID] = map[string]store.Translation{}
		}
		byEntity[translation.EntityID][translation.Field] = translation
	}
	for i := range items {
		found := byEntity[items[i].RequestID]
		if question, ok := found["question"]; ok {
			items[i].QuestionKO = question.Text
			items[i].QuestionKOStale = question.Stale
		}
		if translated, ok := found["options"]; ok {
			var options store.DecisionOptions
			if json.Unmarshal([]byte(translated.Text), &options) == nil && sameOptionKeys(items[i].Options, options.Options) {
				if translated.Stale {
					items[i].OptionsKOStale = true
				} else {
					items[i].OptionsKO = decisionOptionViews(options.Options)
				}
			} else {
				items[i].OptionsKOStale = true
			}
		}
	}
	return jsonResult(map[string]any{"decisions": items, "truncated": truncated})
}

func sameOptionKeys(a []decisionOptionView, b []store.DecisionOption) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
	}
	return true
}

type decisionAnswerInput struct {
	RequestID string `json:"request_id"`
	Option    string `json:"option,omitempty"`
	Text      string `json:"text,omitempty"`
	Note      string `json:"note,omitempty"`
}

func (o operatorTools) decisionAnswer(ctx context.Context, _ *gmcp.CallToolRequest, in decisionAnswerInput) (*gmcp.CallToolResult, any, error) {
	requestID := strings.TrimSpace(in.RequestID)
	if requestID == "" {
		return nil, nil, errors.New("request_id is required")
	}
	option := strings.TrimSpace(in.Option)
	text := strings.TrimSpace(in.Text)
	if note := strings.TrimSpace(in.Note); note != "" {
		if text == "" {
			text = note
		} else {
			text += "\n\n" + note
		}
	}
	if option == "" && text == "" {
		return nil, nil, errors.New("an option or text is required")
	}
	out, err := o.service.Store.ResolveDecision(ctx, store.ResolveDecisionInput{
		RequestID: requestID, Kind: store.DecisionRequestAnswered, Option: option, Text: text,
		Channel: o.principal.decisionChannel(), Actor: o.principal.Actor, Verified: o.principal.Access,
	})
	if err != nil {
		return nil, nil, err
	}
	if out.Refusal != nil {
		return nil, nil, out.Refusal
	}
	result := map[string]any{
		"result":      out.Result,
		"request":     out.Request,
		"actor":       o.principal.Actor,
		"channel":     o.principal.Channel,
		"console_url": taskURL(out.Task.ID),
		"task":        map[string]any{"id": out.Task.ID, "state": out.Task.State, "console_url": taskURL(out.Task.ID)},
	}
	if out.Result == store.DecisionResultPendingConfirmation {
		result["attempt_id"] = out.AttemptID
		result["message"] = "the request needs a verified operator path; this attempt is pending confirmation"
	}
	return jsonResult(result)
}

type taskCommentInput struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

func (o operatorTools) taskComment(ctx context.Context, _ *gmcp.CallToolRequest, in taskCommentInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	if err := store.CheckTaskCommentBody(in.Body); err != nil {
		return nil, nil, err
	}
	comment, err := o.service.CreateTaskComment(ctx, o.principal.Actor, in.ID, in.Body)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{
		"comment": comment, "actor": o.principal.Actor, "channel": o.principal.Channel,
		"console_url": taskURL(in.ID),
	})
}

type taskIntakeInput struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Project string `json:"project"`
	Lane    string `json:"lane,omitempty"`
}

func (o operatorTools) taskIntake(ctx context.Context, _ *gmcp.CallToolRequest, in taskIntakeInput) (*gmcp.CallToolResult, any, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, nil, errors.New("title is required")
	}
	project := strings.TrimSpace(in.Project)
	if project == "" {
		return nil, nil, errors.New("project is required")
	}
	candidates, err := o.duplicateCandidates(ctx, title, strings.TrimSpace(in.Lane))
	if err != nil {
		return nil, nil, err
	}
	task, created, err := o.service.IntakeTask(ctx, intakeActor(o.principal), store.IntakeInput{
		Source: "chatgpt", SourceRef: chatgptSourceRef(title, in.Body, project),
		Title: title, Body: in.Body, Project: &project, Lane: strings.TrimSpace(in.Lane),
	})
	if err != nil {
		return nil, nil, err
	}
	mirrors, err := o.service.Store.ListYouTrackMirrors(ctx, []int64{task.ID})
	if err != nil {
		return nil, nil, err
	}
	mirror, mirrored := mirrors[task.ID]
	return jsonResult(map[string]any{
		"created":              created,
		"task":                 taskSummaryOf(task, mirror, mirrored),
		"duplicate_candidates": candidates,
		"actor":                o.principal.Actor,
		"channel":              o.principal.Channel,
	})
}

// duplicateCandidates reuses the console search to show likely duplicates
// before filing. The store search requires every query word, so extra words in
// the new title can hide an existing shorter title; try a few progressively
// shorter prefixes. The intake path itself stays the existing idempotent one:
// the same (source, source_ref) returns the task already recorded.
func (o operatorTools) duplicateCandidates(ctx context.Context, title, lane string) ([]taskSummary, error) {
	words := strings.Fields(title)
	tasks := make([]store.Task, 0, duplicateCandidateLimit)
	seen := map[int64]bool{}
	for attempt := 0; attempt < 5 && len(words) > 0 && len(tasks) < duplicateCandidateLimit; attempt++ {
		query := strings.Join(words, " ")
		if len(query) > 512 {
			query = strings.ToValidUTF8(query[:512], "")
		}
		results, err := o.service.Search(ctx, query, "tasks", lane, duplicateCandidateWindow)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if len(tasks) == duplicateCandidateLimit {
				break
			}
			id, err := strconv.ParseInt(result.Key, 10, 64)
			if err != nil || seen[id] {
				continue
			}
			seen[id] = true
			task, found, err := o.service.GetTask(ctx, id)
			if err != nil {
				return nil, err
			}
			if !found || !duplicateCandidateState(task.State) {
				continue
			}
			tasks = append(tasks, task)
		}
		if len(words) == 1 {
			break
		}
		words = words[:len(words)-1]
	}
	return o.summaries(ctx, tasks)
}

func duplicateCandidateState(state string) bool {
	return state == store.StateBacklog || state == store.StateUnstarted || store.IsStartedState(state)
}

func chatgptSourceRef(title, body, project string) string {
	sum := sha256.Sum256([]byte(title + "\n" + body + "\n" + project))
	return "chatgpt-" + hex.EncodeToString(sum[:])
}

// intakeActor fits a principal into the store's name alphabet: IntakeTask
// records created_by as a name, and "operator:<email>" is not one. Comments
// and decisions keep the exact operator:<email> actor.
func intakeActor(p principal) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, p.Actor)
	name = strings.Trim(name, "-")
	if len(name) > 128 {
		name = name[:128]
	}
	if name == "" {
		return "operator-chatgpt"
	}
	return name
}

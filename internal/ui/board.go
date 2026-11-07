package ui

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

var boardNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

var boardStates = func() map[string]bool {
	out := make(map[string]bool, len(taskStates))
	for _, state := range taskStates {
		out[state] = true
	}
	return out
}()

// Board endpoints serve the React queue board. They are read-only, same-origin
// BFF routes: the browser never sees hub, bearer, or database credentials.
const (
	boardTasksDefaultLimit = 200
	boardTasksMaxLimit     = 500
	boardDetailRepsLimit   = 500
	policyItemsMax         = 200
	policyPointerKey       = "policy/active"
	policyTitleMax         = 200
	// boardDocRenderMaxBytes is the inline-render budget: a larger document
	// is served as unsupported and the console offers only the raw view.
	boardDocRenderMaxBytes = 256 << 10
)

// Work-board fields are an allowlist: placement account keys, candidate
// telemetry, pane identifiers and raw verdict text never enter the browser.
type boardWorkSession struct {
	JobID  string    `json:"job_id"`
	Host   string    `json:"host"`
	Family string    `json:"family"`
	Model  string    `json:"model"`
	Role   string    `json:"role"`
	Active bool      `json:"active"`
	At     time.Time `json:"at"`
}
type boardWorkBoard struct {
	PrimaryJobID string             `json:"primary_job_id"`
	PR           string             `json:"pr"`
	HeadSHA      string             `json:"head_sha"`
	Sessions     []boardWorkSession `json:"sessions"`
}

func projectWorkBoard(board store.LinearBoard) *boardWorkBoard {
	if len(board.Sessions) == 0 && board.Task.Refs.PR == "" && board.Task.Refs.HeadSHA == "" {
		return nil
	}
	view := &boardWorkBoard{PrimaryJobID: board.PrimaryJob(), PR: board.Task.Refs.PR, HeadSHA: board.Task.Refs.HeadSHA, Sessions: make([]boardWorkSession, 0, len(board.Sessions))}
	for _, session := range board.Sessions {
		if !session.Active && session.JobID != view.PrimaryJobID {
			continue
		}
		view.Sessions = append(view.Sessions, boardWorkSession{JobID: session.JobID, Host: session.Host, Family: session.Family, Model: session.Model, Role: session.Role, Active: session.Active, At: session.At.UTC()})
	}
	return view
}

type boardTask struct {
	ActiveJob  *store.ActiveJobSummary `json:"active_job,omitempty"`
	WorkBoard  *boardWorkBoard         `json:"work_board,omitempty"`
	Command    *boardCommandChip       `json:"command,omitempty"`
	ID         int64                   `json:"id"`
	Lane       string                  `json:"lane"`
	ParentLane string                  `json:"parent_lane,omitempty"`
	Title      string                  `json:"title"`
	Kind       string                  `json:"kind"`
	State      string                  `json:"state"`
	// Category is derived from State; AwaitingDecision is the flag that
	// replaced the needs_decision state. Both feed the console's views.
	Category         string `json:"category"`
	AwaitingDecision bool   `json:"awaiting_decision"`
	ParentID         *int64 `json:"parent_id"`
	// Lifecycle timestamps, null until the task first reaches the stage.
	StartedAt   *time.Time     `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at"`
	CanceledAt  *time.Time     `json:"canceled_at"`
	TriagedAt   *time.Time     `json:"triaged_at"`
	Priority    int            `json:"priority"`
	ClaimedBy   string         `json:"claimed_by,omitempty"`
	CreatedBy   string         `json:"created_by"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Refs        store.TaskRefs `json:"refs"`
	BodyDoc     string         `json:"body_doc,omitempty"`
	// Project is null for legacy rows predating the field and for the
	// (rare) unclassified row — the console groups them as "no project".
	Project *string `json:"project"`
	// Area is the task's routing tag from the closed area vocabulary, null
	// when unset. The console shows it as a read-only label.
	Area *string `json:"area"`
}

type boardTasksResponse struct {
	GeneratedAt time.Time   `json:"generated_at"`
	States      []string    `json:"states"`
	Tasks       []boardTask `json:"tasks"`
	NextAfterID int64       `json:"next_after_id,omitempty"`
	Truncated   bool        `json:"truncated"`
}

func projectBoardTask(task store.Task) boardTask {
	return boardTask{
		ID:               task.ID,
		Lane:             task.Lane,
		ParentLane:       task.ParentLane,
		Title:            task.Title,
		Kind:             task.Kind,
		State:            task.State,
		Category:         task.Category,
		AwaitingDecision: task.AwaitingDecision,
		ParentID:         task.ParentID,
		StartedAt:        task.StartedAt,
		CompletedAt:      task.CompletedAt,
		CanceledAt:       task.CanceledAt,
		TriagedAt:        task.TriagedAt,
		Priority:         task.Priority,
		ClaimedBy:        task.ClaimedBy,
		CreatedBy:        task.CreatedBy,
		CreatedAt:        task.CreatedAt.UTC(),
		UpdatedAt:        task.UpdatedAt.UTC(),
		Refs:             task.Refs,
		BodyDoc:          task.BodyDoc,
		Project:          task.Project,
		Area:             task.Area,
	}
}

// boardTasks pages the complete task set in stable id order. The client owns
// state grouping and priority ordering; the server guarantees a bounded,
// complete traversal via the after_id cursor and reports truncation honestly.
func (h *Handler) boardTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lane := strings.TrimSpace(q.Get("lane"))
	project := strings.TrimSpace(q.Get("project"))
	state := strings.TrimSpace(q.Get("state"))
	parentLane := strings.TrimSpace(q.Get("parent_lane"))
	var afterID int64
	if raw := strings.TrimSpace(q.Get("after_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			http.Error(w, "invalid board query", http.StatusBadRequest)
			return
		}
		afterID = parsed
	}
	limit := boardTasksDefaultLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > boardTasksMaxLimit {
			http.Error(w, "invalid board query", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	if project != "" && !boardNameRE.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if (lane != "" && !boardNameRE.MatchString(lane)) || (parentLane != "" && !boardNameRE.MatchString(parentLane)) || (state != "" && !boardStates[state]) {
		http.Error(w, "invalid board query", http.StatusBadRequest)
		return
	}
	var projectFilter *string
	if project != "" {
		projectFilter = &project
	}
	// One extra row distinguishes "page is full" from "result set is complete".
	tasks, err := h.store.ListTasksPage(r.Context(), lane, state, parentLane, projectFilter, nil, afterID, limit+1)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	truncated := len(tasks) > limit
	if truncated {
		tasks = tasks[:limit]
	}
	response := boardTasksResponse{
		GeneratedAt: time.Now().UTC(),
		States:      taskStates,
		Tasks:       make([]boardTask, 0, len(tasks)),
		Truncated:   truncated,
	}
	workBoards, err := h.store.ReadTaskWorkBoards(r.Context(), tasks)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	activeJobs, err := h.store.ReadActiveJobs(r.Context(), ids)
	if err != nil {
		log.Printf("ui.board_jobs_failed err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	openCommands, err := h.store.ReadOpenOperatorCommands(r.Context(), ids)
	if err != nil {
		log.Printf("ui.board_commands_failed err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	for _, task := range tasks {
		row := projectBoardTask(task)
		row.WorkBoard = projectWorkBoard(workBoards[task.ID])
		if job, ok := activeJobs[task.ID]; ok {
			row.ActiveJob = &job
		}
		if command, ok := openCommands[task.ID]; ok {
			row.Command = &boardCommandChip{ID: command.ID, Kind: command.Kind, State: command.State, CreatedAt: command.CreatedAt.UTC()}
		}
		response.Tasks = append(response.Tasks, row)
		response.NextAfterID = task.ID
	}
	writeBoardJSON(w, response)
}

func (h *Handler) boardProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := h.store.ListTaskProjects(r.Context())
	if err != nil {
		log.Printf("ui.board_projects_failed err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeBoardJSON(w, struct {
		Projects []string `json:"projects"`
	}{projects})
}

type boardJob struct {
	JobID           string             `json:"job_id"`
	Role            string             `json:"role"`
	State           string             `json:"state"`
	Host            string             `json:"host"`
	Family          string             `json:"family"`
	Profile         string             `json:"profile"`
	Model           string             `json:"model"`
	Harness         *string            `json:"harness"`
	Effort          *string            `json:"effort"`
	Skills          []store.JobSkill   `json:"skills"`
	SkillsSupported *bool              `json:"skills_supported"`
	ClaimedAt       *time.Time         `json:"claimed_at"`
	SpawnedAt       *time.Time         `json:"spawned_at"`
	ReadyAt         *time.Time         `json:"ready_at"`
	EndedAt         *time.Time         `json:"ended_at"`
	EndReason       *string            `json:"end_reason"`
	DiffStat        *store.JobDiffStat `json:"diff_stat"`
}

type boardEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	By        string          `json:"by"`
	Note      string          `json:"note,omitempty"`
	NoJob     string          `json:"no_job,omitempty"`
	Field     string          `json:"field,omitempty"`
	FromValue string          `json:"from_value,omitempty"`
	ToValue   string          `json:"to_value,omitempty"`
	Refs      *store.TaskRefs `json:"refs,omitempty"`
	At        time.Time       `json:"at"`
}

type boardCommandChip struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// boardCommand is the console-safe projection of one operator command: the
// ladder timestamps and the instruction text, never arbitrary executor
// payload.
type boardCommand struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	State         string     `json:"state"`
	Intent        string     `json:"intent,omitempty"`
	Text          string     `json:"text,omitempty"`
	Actor         string     `json:"actor"`
	LastErrorCode string     `json:"last_error_code,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at"`
	AckedAt       *time.Time `json:"acked_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
}

func projectBoardCommand(command store.OperatorCommand) boardCommand {
	out := boardCommand{
		ID: command.ID, Kind: command.Kind, State: command.State, Actor: command.Actor,
		LastErrorCode: command.LastErrorCode, CreatedAt: command.CreatedAt.UTC(),
		DeliveredAt: utcOrNil(command.DeliveredAt), AckedAt: utcOrNil(command.AckedAt),
		FinishedAt: utcOrNil(command.FinishedAt), ExpiresAt: command.ExpiresAt.UTC(),
	}
	if command.Kind == store.OperatorCommandKindWorkerMessage {
		var payload store.WorkerMessagePayload
		if err := json.Unmarshal(command.Payload, &payload); err == nil {
			out.Text, out.Intent = payload.Text, payload.Intent
		}
	}
	return out
}

func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

type dwellSegment struct {
	State   string `json:"state"`
	Seconds int64  `json:"seconds"`
	Open    bool   `json:"open"`
}

type boardLinear struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier"`
}

type participantSegment struct {
	Role          *string `json:"role"`
	ModelID       *string `json:"model_id"`
	Reps          int     `json:"reps"`
	Rounds        *int    `json:"rounds"`
	BlockersFound *int    `json:"blockers_found"`
	Completed     *int    `json:"completed"`
	InputTokens   *int64  `json:"input_tokens"`
	OutputTokens  *int64  `json:"output_tokens"`
}

// boardParticipants is the exact task_ref telemetry projection. Coverage is
// an explicit state, never a guessed attribution: no reps means
// "not_collected", and an unrecorded model or role stays null rather than
// being inferred from task events, job names, or the last writer. When the
// rep query hits its bound, truncated is set and the segment totals are
// explicitly partial — they never masquerade as a complete sum.
type boardParticipants struct {
	TaskRef   string               `json:"task_ref"`
	Coverage  string               `json:"coverage"`
	Truncated bool                 `json:"truncated,omitempty"`
	Segments  []participantSegment `json:"segments"`
}

type boardDocument struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	OriginClass string `json:"origin_class"`
}

type boardDetailResponse struct {
	Documents          []boardDocument    `json:"documents"`
	DocumentsTruncated bool               `json:"documents_truncated"`
	Origins            []store.TaskOrigin `json:"origins"`
	Changes            []store.Change     `json:"changes"`
	Jobs               []boardJob         `json:"jobs"`
	Commands           []boardCommand     `json:"commands"`
	OriginsTruncated   bool               `json:"origins_truncated"`
	ChangesTruncated   bool               `json:"changes_truncated"`
	JobsTruncated      bool               `json:"jobs_truncated"`
	CommandsTruncated  bool               `json:"commands_truncated"`
	Task               boardTask          `json:"task"`
	Events             []boardEvent       `json:"events"`
	Dwell              []dwellSegment     `json:"dwell"`
	Linear             *boardLinear       `json:"linear"`
	Participants       boardParticipants  `json:"participants"`
	// DecisionRequests is every structured request of the task, newest
	// first, with its derived display state (#618). Always present (possibly
	// empty) so a client can tell "none recorded" from an older server.
	DecisionRequests []decisionRequestView `json:"decision_requests"`
	DecisionLegacy   *legacyDecisionView   `json:"decision_legacy,omitempty"`
	// Children and Relations come from GetTask; absent when none.
	Children  []store.TaskChild    `json:"children,omitempty"`
	Relations []store.TaskRelation `json:"relations,omitempty"`
}

func taskRef(id int64) string {
	return "hk:task/" + strconv.FormatInt(id, 10)
}

// taskDwell totals the time spent in each canonical state. Relane, project
// and area events carry classification names, not states, and decision
// events record a request without a state change, so all four are
// excluded — none may end the open dwell segment. The final segment stays
// open: its seconds run from the last transition to now.
func taskDwell(task store.Task, now time.Time) []dwellSegment {
	totals := map[string]int64{}
	current, start := "backlog", task.CreatedAt
	for _, event := range task.Events {
		if event.Kind != store.TaskEventTransition && event.Kind != store.TaskEventStateMigration {
			continue
		}
		if event.At.After(start) {
			totals[current] += int64(event.At.Sub(start).Seconds())
		}
		current, start = event.To, event.At
	}
	if now.After(start) {
		totals[current] += int64(now.Sub(start).Seconds())
	}
	out := make([]dwellSegment, 0, len(totals))
	for _, state := range dwellStates {
		seconds, ok := totals[state]
		if !ok {
			continue
		}
		out = append(out, dwellSegment{State: state, Seconds: seconds, Open: state == current})
	}
	return out
}

func projectParticipants(taskID int64, reps []store.BenchRep) boardParticipants {
	view := boardParticipants{TaskRef: taskRef(taskID), Coverage: "not_collected", Segments: []participantSegment{}}
	if len(reps) == 0 {
		return view
	}
	view.Coverage = "collected"
	type segmentKey struct{ role, model string }
	index := map[segmentKey]int{}
	keys := []segmentKey{}
	for _, rep := range reps {
		var role, model string
		if rep.Role != nil {
			role = *rep.Role
		}
		if rep.ModelID != nil {
			model = *rep.ModelID
		}
		key := segmentKey{role: role, model: model}
		at, ok := index[key]
		if !ok {
			at = len(view.Segments)
			index[key] = at
			keys = append(keys, key)
			view.Segments = append(view.Segments, participantSegment{Role: rep.Role, ModelID: rep.ModelID})
		}
		segment := &view.Segments[at]
		segment.Reps++
		segment.Rounds = sumInt(segment.Rounds, rep.Rounds)
		segment.BlockersFound = sumInt(segment.BlockersFound, rep.BlockersFound)
		segment.Completed = sumInt(segment.Completed, rep.Completed)
		segment.InputTokens = sumInt64(segment.InputTokens, rep.InputTokens)
		segment.OutputTokens = sumInt64(segment.OutputTokens, rep.OutputTokens)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].role != keys[j].role {
			return keys[i].role < keys[j].role
		}
		return keys[i].model < keys[j].model
	})
	ordered := make([]participantSegment, 0, len(view.Segments))
	for _, key := range keys {
		ordered = append(ordered, view.Segments[index[key]])
	}
	view.Segments = ordered
	return view
}

func sumInt(total *int, value *int) *int {
	if value == nil {
		return total
	}
	if total == nil {
		out := *value
		return &out
	}
	*total += *value
	return total
}

func sumInt64(total *int64, value *int64) *int64 {
	if value == nil {
		return total
	}
	if total == nil {
		out := *value
		return &out
	}
	*total += *value
	return total
}

func (h *Handler) boardTaskDetail(w http.ResponseWriter, r *http.Request) {
	rawID := strings.TrimPrefix(r.URL.Path, "/ui/api/board/tasks/")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	task, found, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	origins, err := h.store.ListTaskOriginsPage(r.Context(), id, 101)
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	changes, err := h.store.ListChangesByTask(r.Context(), id, 101)
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	jobs, err := h.store.ListJobsByTask(r.Context(), id, 101)
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	documents, err := h.store.ListDocuments(r.Context(), "", "", "", id, 101)
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	documentsTruncated := len(documents) > 100
	if documentsTruncated {
		documents = documents[:100]
	}
	projectedDocuments := make([]boardDocument, 0, len(documents))
	for _, document := range documents {
		projectedDocuments = append(projectedDocuments, boardDocument{Key: document.Key, Kind: document.Kind, OriginClass: document.OriginClass})
	}
	originsTruncated, changesTruncated, jobsTruncated := len(origins) > 100, len(changes) > 100, len(jobs) > 100
	if originsTruncated {
		origins = origins[:100]
	}
	if changesTruncated {
		changes = changes[:100]
	}
	if jobsTruncated {
		jobs = jobs[:100]
	}
	if origins == nil {
		origins = []store.TaskOrigin{}
	}
	if changes == nil {
		changes = []store.Change{}
	}
	projectedJobs := make([]boardJob, 0, len(jobs))
	for _, j := range jobs {
		projectedJobs = append(projectedJobs, boardJob{j.JobID, j.Role, j.State, j.Host, j.Family, j.Profile, j.Model, j.Harness, j.Effort, j.Skills, j.SkillsSupported, j.ClaimedAt, j.SpawnedAt, j.ReadyAt, j.EndedAt, j.EndReason, j.DiffStat})
	}
	commands, err := h.store.ListOperatorCommandsByTask(r.Context(), id, 101)
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	commandsTruncated := len(commands) > 100
	if commandsTruncated {
		commands = commands[:100]
	}
	projectedCommands := make([]boardCommand, 0, len(commands))
	for _, command := range commands {
		projectedCommands = append(projectedCommands, projectBoardCommand(command))
	}
	now := time.Now().UTC()
	response := boardDetailResponse{
		Documents: projectedDocuments, DocumentsTruncated: documentsTruncated,
		Origins: origins, Changes: changes, Jobs: projectedJobs, OriginsTruncated: originsTruncated, ChangesTruncated: changesTruncated, JobsTruncated: jobsTruncated,
		Commands: projectedCommands, CommandsTruncated: commandsTruncated,
		Task:      projectBoardTask(task),
		Events:    make([]boardEvent, 0, len(task.Events)),
		Dwell:     taskDwell(task, now),
		Linear:    nil,
		Children:  task.Children,
		Relations: task.Relations,
	}
	workBoards, err := h.store.ReadTaskWorkBoards(r.Context(), []store.Task{task})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	response.Task.WorkBoard = projectWorkBoard(workBoards[task.ID])
	activeJobs, err := h.store.ReadActiveJobs(r.Context(), []int64{task.ID})
	if err != nil {
		log.Printf("ui.board_detail_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if job, ok := activeJobs[task.ID]; ok {
		response.Task.ActiveJob = &job
	}
	response.DecisionRequests, response.DecisionLegacy = taskDecisionViews(task, now)
	for _, event := range task.Events {
		response.Events = append(response.Events, boardEvent{
			ID: event.ID, Kind: event.Kind, From: event.From, To: event.To, By: event.By,
			Note: event.Note, NoJob: event.NoJob, Field: event.Field, FromValue: event.FromValue, ToValue: event.ToValue, Refs: event.Refs, At: event.At.UTC(),
		})
	}
	issue, found, err := h.store.GetLinearIssue(r.Context(), id)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if found {
		response.Linear = &boardLinear{IssueID: issue.IssueID, Identifier: issue.Identifier}
	}
	// One extra row distinguishes "reps are complete" from "the bound was
	// hit". Beyond the bound the totals are partial and must say so.
	reps, err := h.store.ListBenchRepsByTaskRef(r.Context(), taskRef(id), boardDetailRepsLimit+1)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	truncated := len(reps) > boardDetailRepsLimit
	if truncated {
		reps = reps[:boardDetailRepsLimit]
	}
	response.Participants = projectParticipants(id, reps)
	response.Participants.Truncated = truncated
	writeBoardJSON(w, response)
}

// boardDocResponse is the read-only document payload behind the task
// overview's inline body. Format tells the console whether it may hand the
// body to the markdown renderer; anything else is shown only as raw text.
type boardDocResponse struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
	Bytes     int       `json:"bytes"`
	Format    string    `json:"format"`
	Reason    string    `json:"reason,omitempty"`
	Body      string    `json:"body"`
}

// boardDocFormat classifies a document body for inline rendering. Only
// UTF-8 text within the render budget that is not a JSON value is markdown.
func boardDocFormat(body string) (string, string) {
	switch {
	case !utf8.ValidString(body):
		return "unsupported", "not_text"
	case len(body) > boardDocRenderMaxBytes:
		return "unsupported", "too_large"
	}
	trimmed := strings.TrimSpace(body)
	if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
		return "unsupported", "json"
	}
	return "markdown", ""
}

// boardDoc serves one hk document by exact key for the task overview. It
// reads documents only: the key is shape-checked like /ui/doc, never joined
// to a filesystem path, and report_path is never resolved here.
func (h *Handler) boardDoc(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !validDocumentKey(key) {
		http.Error(w, "invalid document key", http.StatusBadRequest)
		return
	}
	document, found, err := h.store.GetDocument(r.Context(), key)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	format, reason := boardDocFormat(document.Body)
	writeBoardJSON(w, boardDocResponse{
		Key:       document.Key,
		Kind:      document.Kind,
		SHA256:    document.SHA256,
		UpdatedAt: document.UpdatedAt.UTC(),
		Bytes:     len(document.Body),
		Format:    format,
		Reason:    reason,
		Body:      strings.ToValidUTF8(document.Body, "\uFFFD"),
	})
}

// Policy canon is resolved from two exact document keys. The pointer document
// policy/active holds the exact manifest document key; the manifest document
// holds a JSON item list. Anything else is exploration, never coverage, so the
// endpoint reports an explicit status instead of scanning prefixes.
type policyResponse struct {
	Status        string       `json:"status"`
	PointerKey    string       `json:"pointer_key"`
	PointerDocURL string       `json:"pointer_doc_url"`
	ManifestKey   string       `json:"manifest_key,omitempty"`
	ManifestURL   string       `json:"manifest_doc_url,omitempty"`
	Release       string       `json:"release,omitempty"`
	Items         []policyItem `json:"items"`
	Truncated     bool         `json:"truncated,omitempty"`
}

type policyItem struct {
	Key    string `json:"key"`
	Title  string `json:"title,omitempty"`
	DocURL string `json:"doc_url"`
	Exists bool   `json:"exists"`
}

type policyManifest struct {
	Release string `json:"release"`
	Items   []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"items"`
}

func (h *Handler) policyActive(w http.ResponseWriter, r *http.Request) {
	response := policyResponse{Status: "not_configured", PointerKey: policyPointerKey, PointerDocURL: "/ui/doc/" + policyPointerKey, Items: []policyItem{}}
	pointer, found, err := h.store.GetDocument(r.Context(), policyPointerKey)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		writeBoardJSON(w, response)
		return
	}
	manifestKey := strings.TrimSpace(pointer.Body)
	if !validDocumentKey(manifestKey) {
		response.Status = "invalid_pointer"
		writeBoardJSON(w, response)
		return
	}
	response.ManifestKey = manifestKey
	response.ManifestURL = "/ui/doc/" + manifestKey
	manifest, found, err := h.store.GetDocument(r.Context(), manifestKey)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		response.Status = "manifest_missing"
		writeBoardJSON(w, response)
		return
	}
	var parsed policyManifest
	decoder := json.NewDecoder(strings.NewReader(manifest.Body))
	var trailing any
	if err := decoder.Decode(&parsed); err != nil || decoder.Decode(&trailing) != io.EOF {
		response.Status = "invalid_manifest"
		writeBoardJSON(w, response)
		return
	}
	if len(parsed.Items) > policyItemsMax {
		parsed.Items = parsed.Items[:policyItemsMax]
		response.Truncated = true
	}
	for _, item := range parsed.Items {
		if !validDocumentKey(item.Key) || len(item.Title) > policyTitleMax {
			response.Status = "invalid_manifest"
			response.Items = []policyItem{}
			writeBoardJSON(w, response)
			return
		}
	}
	response.Release = parsed.Release
	response.Status = "ok"
	for _, item := range parsed.Items {
		entry := policyItem{Key: item.Key, Title: item.Title, DocURL: "/ui/doc/" + item.Key}
		_, entry.Exists, err = h.store.GetDocument(r.Context(), item.Key)
		if err != nil {
			http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
			return
		}
		response.Items = append(response.Items, entry)
	}
	writeBoardJSON(w, response)
}

func writeBoardJSON(w http.ResponseWriter, response any) {
	writeBoardJSONStatus(w, http.StatusOK, response)
}

// writeJSONError writes the console's JSON error body, {"error": code}, the
// same shape the /v1 API uses.
func writeJSONError(w http.ResponseWriter, status int, code string) {
	writeBoardJSONStatus(w, status, map[string]string{"error": code})
}

// writeBoardJSONStatus is the one JSON writer for console API responses.
func writeBoardJSONStatus(w http.ResponseWriter, status int, response any) {
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

package ui

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
)

type boardTask struct {
	ID         int64          `json:"id"`
	Lane       string         `json:"lane"`
	ParentLane string         `json:"parent_lane,omitempty"`
	Title      string         `json:"title"`
	Kind       string         `json:"kind"`
	State      string         `json:"state"`
	Priority   int            `json:"priority"`
	ClaimedBy  string         `json:"claimed_by,omitempty"`
	CreatedBy  string         `json:"created_by"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Refs       store.TaskRefs `json:"refs"`
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
		ID:         task.ID,
		Lane:       task.Lane,
		ParentLane: task.ParentLane,
		Title:      task.Title,
		Kind:       task.Kind,
		State:      task.State,
		Priority:   task.Priority,
		ClaimedBy:  task.ClaimedBy,
		CreatedBy:  task.CreatedBy,
		CreatedAt:  task.CreatedAt.UTC(),
		UpdatedAt:  task.UpdatedAt.UTC(),
		Refs:       task.Refs,
	}
}

// boardTasks pages the complete task set in stable id order. The client owns
// state grouping and priority ordering; the server guarantees a bounded,
// complete traversal via the after_id cursor and reports truncation honestly.
func (h *Handler) boardTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lane := strings.TrimSpace(q.Get("lane"))
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
	if (lane != "" && !boardNameRE.MatchString(lane)) || (parentLane != "" && !boardNameRE.MatchString(parentLane)) || (state != "" && !boardStates[state]) {
		http.Error(w, "invalid board query", http.StatusBadRequest)
		return
	}
	// One extra row distinguishes "page is full" from "result set is complete".
	tasks, err := h.store.ListTasksPage(r.Context(), lane, state, parentLane, afterID, limit+1)
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
	for _, task := range tasks {
		response.Tasks = append(response.Tasks, projectBoardTask(task))
		response.NextAfterID = task.ID
	}
	writeBoardJSON(w, response)
}

type boardEvent struct {
	ID   int64           `json:"id"`
	From string          `json:"from"`
	To   string          `json:"to"`
	By   string          `json:"by"`
	Note string          `json:"note,omitempty"`
	Refs *store.TaskRefs `json:"refs,omitempty"`
	At   time.Time       `json:"at"`
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
// being inferred from task events, job names, or the last writer.
type boardParticipants struct {
	TaskRef  string               `json:"task_ref"`
	Coverage string               `json:"coverage"`
	Segments []participantSegment `json:"segments"`
}

type boardDetailResponse struct {
	Task         boardTask         `json:"task"`
	Events       []boardEvent      `json:"events"`
	Dwell        []dwellSegment    `json:"dwell"`
	Linear       *boardLinear      `json:"linear"`
	Participants boardParticipants `json:"participants"`
}

func taskRef(id int64) string {
	return "hk:task/" + strconv.FormatInt(id, 10)
}

// taskDwell totals the time spent in each canonical state. The final segment
// stays open: its seconds run from the last transition to now.
func taskDwell(task store.Task, now time.Time) []dwellSegment {
	totals := map[string]int64{}
	current, start := "backlog", task.CreatedAt
	for _, event := range task.Events {
		if event.At.After(start) {
			totals[current] += int64(event.At.Sub(start).Seconds())
		}
		current, start = event.To, event.At
	}
	if now.After(start) {
		totals[current] += int64(now.Sub(start).Seconds())
	}
	out := make([]dwellSegment, 0, len(totals))
	for _, state := range taskStates {
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
	response := boardDetailResponse{
		Task:   projectBoardTask(task),
		Events: make([]boardEvent, 0, len(task.Events)),
		Dwell:  taskDwell(task, time.Now().UTC()),
		Linear: nil,
	}
	for _, event := range task.Events {
		response.Events = append(response.Events, boardEvent{
			ID: event.ID, From: event.From, To: event.To, By: event.By,
			Note: event.Note, Refs: event.Refs, At: event.At.UTC(),
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
	reps, err := h.store.ListBenchRepsByTaskRef(r.Context(), taskRef(id), boardDetailRepsLimit)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	response.Participants = projectParticipants(id, reps)
	writeBoardJSON(w, response)
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
	if err := decoder.Decode(&parsed); err != nil {
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
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// reap.go — GET /ui/api/reap is the read-only "정리 후보" (cleanup candidates)
// view of task #603 stage 1. Nodes judge their own panes and report through
// the panewire hub (GET /v1/session-reap, panewire hub_session_reap.go); this
// endpoint joins that report with task state, which only handoffkeep owns.
//
// Nothing here closes a session. The join can only keep or lower a node
// verdict, with one exception owned by this file: a node never proposes a
// builder, it reports "builder-task-gate", and a builder becomes a candidate
// here only when exactly one task has refs.job_id equal to its job (exact
// equality), that task is merged or dropped, and it has been so for longer
// than the node's grace. Every other outcome is held with a reason.

const (
	reapClassCandidate       = "candidate"
	reapClassBuilderTaskGate = "builder-task-gate"
	reapClassHeld            = "held"
)

type hubReapReport struct {
	Schema       int                `json:"schema"`
	GeneratedAt  string             `json:"generated_at"`
	GraceSeconds int64              `json:"grace_seconds"`
	Observed     bool               `json:"observed"`
	JobsReadable bool               `json:"jobs_readable"`
	Truncated    bool               `json:"truncated"`
	Rows         []hubReapRow       `json:"rows"`
	Summary      hubReapNodeSummary `json:"summary"`
}

type hubReapRow struct {
	PaneID             string `json:"pane_id"`
	TabID              string `json:"tab_id,omitempty"`
	WorkspaceID        string `json:"workspace_id,omitempty"`
	AgentName          string `json:"agent_name,omitempty"`
	Status             string `json:"status"`
	JobID              string `json:"job_id"`
	OwnerLane          string `json:"owner_lane,omitempty"`
	Role               string `json:"role,omitempty"`
	Class              string `json:"class"`
	Reason             string `json:"reason,omitempty"`
	TerminalKind       string `json:"terminal_kind,omitempty"`
	TerminalAt         string `json:"terminal_at,omitempty"`
	TerminalAgeSeconds *int64 `json:"terminal_age_seconds,omitempty"`
}

type hubReapNodeSummary struct {
	Panes           int `json:"panes"`
	NoJob           int `json:"no_job"`
	Candidate       int `json:"candidate"`
	BuilderTaskGate int `json:"builder_task_gate"`
	Held            int `json:"held"`
}

type hubReapNode struct {
	MachineID  string        `json:"machine_id"`
	State      string        `json:"state"`
	Stale      bool          `json:"stale"`
	ReceivedAt string        `json:"received_at"`
	Report     hubReapReport `json:"report"`
}

type reapResponse struct {
	GeneratedAt    string        `json:"generated_at"`
	Status         string        `json:"status"`
	FetchedAt      string        `json:"fetched_at"`
	Candidates     []reapRow     `json:"candidates"`
	Held           []reapRow     `json:"held"`
	Nodes          []reapNodeRow `json:"nodes"`
	TasksTruncated bool          `json:"tasks_truncated,omitempty"`
}

// reapRow is one pane as the console shows it. Basis names why a candidate
// is one: "job-terminal" (a worker whose job ended) or "task-terminal" (a
// builder whose linked task is merged or dropped).
type reapRow struct {
	Machine      string `json:"machine"`
	PaneID       string `json:"pane_id"`
	TabID        string `json:"tab_id,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
	Status       string `json:"status"`
	JobID        string `json:"job_id"`
	OwnerLane    string `json:"owner_lane,omitempty"`
	Role         string `json:"role,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Basis        string `json:"basis,omitempty"`
	TerminalKind string `json:"terminal_kind,omitempty"`
	TerminalAt   string `json:"terminal_at,omitempty"`
	TaskID       *int64 `json:"task_id,omitempty"`
	TaskState    string `json:"task_state,omitempty"`
}

type reapNodeRow struct {
	MachineID    string             `json:"machine_id"`
	State        string             `json:"state"`
	Stale        bool               `json:"stale"`
	ReceivedAt   string             `json:"received_at"`
	GeneratedAt  string             `json:"generated_at"`
	GraceSeconds int64              `json:"grace_seconds"`
	Observed     bool               `json:"observed"`
	JobsReadable bool               `json:"jobs_readable"`
	Truncated    bool               `json:"truncated"`
	Summary      hubReapNodeSummary `json:"summary"`
}

// projectReap is the pure join: hub report rows plus tasks in, console rows
// out. It never raises a worker verdict, and a builder can rise only through
// the task gate below.
func projectReap(nodes []hubReapNode, tasks []store.Task, tasksTruncated bool, now time.Time) ([]reapRow, []reapRow, []reapNodeRow) {
	byJob := make(map[string][]store.Task)
	for _, task := range tasks {
		if task.Refs.JobID != "" {
			byJob[task.Refs.JobID] = append(byJob[task.Refs.JobID], task)
		}
	}
	candidates, held := []reapRow{}, []reapRow{}
	nodeRows := make([]reapNodeRow, 0, len(nodes))
	for _, node := range nodes {
		report := node.Report
		nodeRows = append(nodeRows, reapNodeRow{
			MachineID: node.MachineID, State: node.State, Stale: node.Stale, ReceivedAt: node.ReceivedAt,
			GeneratedAt: report.GeneratedAt, GraceSeconds: report.GraceSeconds, Observed: report.Observed,
			JobsReadable: report.JobsReadable, Truncated: report.Truncated, Summary: report.Summary,
		})
		grace := time.Duration(report.GraceSeconds) * time.Second
		for _, source := range report.Rows {
			row := reapRow{
				Machine: node.MachineID, PaneID: source.PaneID, TabID: source.TabID, AgentName: source.AgentName,
				Status: source.Status, JobID: source.JobID, OwnerLane: source.OwnerLane, Role: source.Role,
				Reason: source.Reason, TerminalKind: source.TerminalKind, TerminalAt: source.TerminalAt,
			}
			class := source.Class
			switch {
			case class != reapClassCandidate && class != reapClassBuilderTaskGate:
				if row.Reason == "" {
					row.Reason = "unknown-class"
				}
				class = reapClassHeld
			case node.Stale || node.State != "connected":
				// The pane may have changed since the node last reported.
				class, row.Reason = reapClassHeld, "node-not-connected"
			case !report.Observed || !report.JobsReadable:
				class, row.Reason = reapClassHeld, "node-unobserved"
			case class == reapClassCandidate:
				if source.Role != "worker" || source.TerminalKind == "" {
					class, row.Reason = reapClassHeld, "candidate-shape"
				} else {
					row.Basis = "job-terminal"
				}
			default:
				class, row.Reason = reapBuilderTaskGate(&row, byJob[source.JobID], tasksTruncated, grace, now)
			}
			if class == reapClassCandidate {
				candidates = append(candidates, row)
			} else {
				held = append(held, row)
			}
		}
	}
	for _, rows := range [][]reapRow{candidates, held} {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Machine != rows[j].Machine {
				return rows[i].Machine < rows[j].Machine
			}
			return rows[i].PaneID < rows[j].PaneID
		})
	}
	return candidates, held, nodeRows
}

// reapBuilderTaskGate decides a builder row. The grace is measured from the
// task's updated_at, which is never earlier than the terminal transition, so
// a later edit only lengthens the wait.
func reapBuilderTaskGate(row *reapRow, linked []store.Task, tasksTruncated bool, grace time.Duration, now time.Time) (string, string) {
	if row.Role != "builder" {
		return reapClassHeld, "builder-shape"
	}
	if tasksTruncated {
		return reapClassHeld, "tasks-truncated"
	}
	switch len(linked) {
	case 0:
		return reapClassHeld, "task-unlinked"
	case 1:
	default:
		return reapClassHeld, "task-ambiguous"
	}
	task := linked[0]
	id := task.ID
	row.TaskID, row.TaskState = &id, task.State
	if task.State != "merged" && task.State != "dropped" {
		return reapClassHeld, "task-open"
	}
	if task.UpdatedAt.IsZero() || now.Sub(task.UpdatedAt) < grace {
		return reapClassHeld, "task-within-grace"
	}
	row.Basis = "task-terminal"
	return reapClassCandidate, ""
}

func (p *hubProxy) sessionReap(ctx context.Context) ([]hubReapNode, string) {
	if !p.configured() {
		return nil, "unconfigured"
	}
	var wrapped struct {
		Nodes []hubReapNode `json:"nodes"`
	}
	status, err := p.getJSON(ctx, "/v1/session-reap", &wrapped)
	switch {
	case err == nil:
		return wrapped.Nodes, "ok"
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		// A hub from before #603 has no such endpoint.
		return nil, "unsupported"
	default:
		return nil, classifyUpstream(status, err)
	}
}

func (h *Handler) reapAPI(w http.ResponseWriter, r *http.Request) {
	nodes, status := h.hub.sessionReap(r.Context())
	now := time.Now().UTC()
	response := reapResponse{GeneratedAt: now.Format(time.RFC3339), Status: status, Candidates: []reapRow{}, Held: []reapRow{}, Nodes: []reapNodeRow{}}
	if status == "ok" {
		tasks, truncated, err := h.liveTasks(r.Context())
		if err != nil {
			http.Error(w, "reap view unavailable", http.StatusInternalServerError)
			return
		}
		response.FetchedAt = response.GeneratedAt
		response.TasksTruncated = truncated
		response.Candidates, response.Held, response.Nodes = projectReap(nodes, tasks, truncated, now)
	}
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "reap view unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

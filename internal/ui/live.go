package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// live.go — GET /ui/api/live is a read-only aggregation for the two live views
// of task #598: live job chips on queue rows, and a machine/session/load view.
// It joins three inputs server-side so the browser never sees hub credentials:
//
//   - hub /v1/jobs  (panewire hub_jobs.go hubConsoleJob, ~line 414)
//   - hub /v1/nodes (panewire hub.go HubNode: load, memory, session_snapshot)
//   - tasks.refs.job_id (exact-equality joins only — never prefix/substring)
//
// Every section reports its own status and the timestamp of the data actually
// served, so a hub failure (timeout, 401, 5xx) can never render as an empty or
// idle fleet, and a stale snapshot can never render as "한가함". Nil hub
// measurements stay nil end-to-end — they are never rewritten to zero.

const liveFailCacheTTL = 2 * time.Second

// liveTaskScanMax bounds the paged task scan. The scan covers every task so a
// job can be attributed to a task in any state; exceeding the bound marks the
// response truncated rather than silently dropping linkage.
const liveTaskScanMax = 20000

// liveStates are the task states that show live chips and participate in the
// "task without an active job" mismatch: claimed, in_progress, verifying.
var liveStates = map[string]bool{"claimed": true, "in_progress": true, "verifying": true}

type liveResponse struct {
	GeneratedAt    string           `json:"generated_at"`
	Jobs           liveJobsSection  `json:"jobs"`
	Nodes          liveNodesSection `json:"nodes"`
	Links          []liveTaskLink   `json:"links"`
	Mismatch       liveMismatch     `json:"mismatch"`
	TasksTruncated bool             `json:"tasks_truncated,omitempty"`
}

// liveJobsSection / liveNodesSection carry an explicit status so failure modes
// (timeout, auth_failed, http_error, unsupported, unconfigured) stay distinct
// from an empty result. FetchedAt is the timestamp of the data actually served:
// while a section degrades to last-good items it keeps the last successful
// fetch time, and it is empty only when the hub has never answered.
type liveJobsSection struct {
	Status    string    `json:"status"`
	FetchedAt string    `json:"fetched_at"`
	Items     []liveJob `json:"items"`
}

type liveNodesSection struct {
	Status    string     `json:"status"`
	FetchedAt string     `json:"fetched_at"`
	Items     []liveNode `json:"items"`
}

// liveJob mirrors panewire hub_jobs.go hubConsoleJob (the /v1/jobs item).
type liveJob struct {
	Machine       string `json:"machine"`
	JobID         string `json:"job_id"`
	OwnerLane     string `json:"owner_lane,omitempty"`
	Pane          string `json:"pane,omitempty"`
	Tier          string `json:"tier,omitempty"`
	Role          string `json:"role,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	LastEventKind string `json:"last_event_kind,omitempty"`
	LastEventAt   string `json:"last_event_at,omitempty"`
}

// liveNode mirrors panewire hub.go HubNode. Load, Memory and SessionSnapshot
// stay pointer-typed so an unmeasured value reaches the client as null rather
// than a fabricated zero. ActiveJobs is nil while the jobs section has never
// produced data — an unknown count, not a zero count.
type liveNode struct {
	MachineID          string        `json:"machine_id"`
	State              string        `json:"state"`
	AcceptingEffective bool          `json:"accepting_effective"`
	LastPingMS         *int64        `json:"last_ping_ms"`
	Load               *liveLoad     `json:"load"`
	Memory             *liveMemory   `json:"memory"`
	SessionSnapshot    *liveSnapshot `json:"session_snapshot"`
	DisplayState       string        `json:"display_state"`
	ActiveJobs         *int          `json:"active_jobs"`
}

// liveLoad mirrors panewire hub.go HubNodeLoad.
type liveLoad struct {
	Load1  *float64 `json:"load1"`
	Load5  *float64 `json:"load5"`
	Load15 *float64 `json:"load15"`
	NCPU   *int     `json:"ncpu"`
}

// liveMemory mirrors panewire checks.go HubHostMemory. Source tells the client
// how free_pct was measured; vm_stat percentages are not usable memory and the
// client renders them as "측정 불가", never as a percentage.
type liveMemory struct {
	FreePct      *float64 `json:"free_pct"`
	CompressedMB *float64 `json:"compressed_mb"`
	SwapUsedMB   *float64 `json:"swap_used_mb"`
	PSISomeAvg10 *float64 `json:"psi_some_avg10"`
	Source       string   `json:"source"`
}

// liveSnapshot mirrors panewire session_snapshot.go HubSessionSnapshot.
type liveSnapshot struct {
	Sessions       []liveSession `json:"sessions"`
	SnapshotStatus string        `json:"snapshot_status"`
	Truncated      bool          `json:"truncated"`
	ReceivedAt     string        `json:"received_at"`
	Stale          bool          `json:"stale"`
}

// liveSession mirrors panewire session_snapshot.go HubSession. Sessions expose
// label/status/pane/workspace metadata only — the snapshot never carries pane
// content, so trading sessions can be listed without leaking their prompts.
type liveSession struct {
	PaneID           string `json:"pane_id"`
	WorkspaceID      string `json:"workspace_id"`
	Label            string `json:"label"`
	AgentName        string `json:"agent_name,omitempty"`
	DisplayLabel     string `json:"display_label,omitempty"`
	Status           string `json:"status"`
	InteractiveReady *bool  `json:"interactive_ready,omitempty"`
	Revision         int64  `json:"revision"`
	StateChangeSeq   int64  `json:"state_change_seq"`
}

// liveTaskLink joins one task to hub jobs. JobID is the task's own
// refs.job_id. Children are the one-hop owner_lane siblings (tester/worker
// jobs spawned with --owner <builder lane>); no deeper chain is followed.
type liveTaskLink struct {
	TaskID   int64    `json:"task_id"`
	JobID    string   `json:"job_id"`
	JobFound bool     `json:"job_found"`
	Children []string `json:"children"`
}

type liveMismatch struct {
	// Basis records what the comparison ran against:
	//   current     — the jobs list was fetched this round
	//   cached      — the jobs section is serving last-good data (status != ok)
	//   unavailable — the hub has never answered /v1/jobs; both lists below are
	//                 empty by definition and must not be read as "no mismatch"
	Basis           string   `json:"basis"`
	TasksWithoutJob []int64  `json:"tasks_without_job"`
	JobsWithoutTask []string `json:"jobs_without_task"`
}

// liveJobsCache / liveNodesCache hold the last computed section plus the last
// known-good one. A failed fetch degrades to the last-good items while keeping
// the failure status and the stale fetched_at — never an empty idle list.
type liveJobsCache struct {
	last liveJobsSection
	at   time.Time
	ok   *liveJobsSection
}

type liveNodesCache struct {
	last liveNodesSection
	at   time.Time
	ok   *liveNodesSection
}

func (h *Handler) liveAPI(w http.ResponseWriter, r *http.Request) {
	jobs := h.hub.liveJobsNow(r.Context())
	nodes := h.hub.liveNodesNow(r.Context())
	tasks, truncated, err := h.liveTasks(r.Context())
	if err != nil {
		http.Error(w, "live view unavailable", http.StatusInternalServerError)
		return
	}

	byJob := make(map[string]liveJob, len(jobs.Items))
	for _, job := range jobs.Items {
		byJob[job.JobID] = job
	}
	jobsKnown := jobs.FetchedAt != ""
	if jobsKnown {
		for i := range nodes.Items {
			count := 0
			for _, job := range jobs.Items {
				if job.Machine == nodes.Items[i].MachineID {
					count++
				}
			}
			nodes.Items[i].ActiveJobs = &count
		}
	}

	links := make([]liveTaskLink, 0)
	mismatch := liveMismatch{
		Basis:           "unavailable",
		TasksWithoutJob: []int64{},
		JobsWithoutTask: []string{},
	}
	if jobsKnown {
		if jobs.Status == "ok" {
			mismatch.Basis = "current"
		} else {
			mismatch.Basis = "cached"
		}
	}

	connectedLanes := map[string]bool{}
	claimedJobIDs := map[string]bool{}
	for _, task := range tasks {
		jobID := task.Refs.JobID
		found := false
		var primary liveJob
		if jobID != "" {
			claimedJobIDs[jobID] = true
			primary, found = byJob[jobID]
			link := liveTaskLink{TaskID: task.ID, JobID: jobID, JobFound: found, Children: []string{}}
			if found {
				if primary.OwnerLane != "" {
					connectedLanes[primary.OwnerLane] = true
				}
				for _, job := range jobs.Items {
					if job.JobID != jobID && job.OwnerLane != "" && job.OwnerLane == primary.OwnerLane {
						link.Children = append(link.Children, job.JobID)
					}
				}
				sort.Strings(link.Children)
			}
			links = append(links, link)
		}
		// A live-state task with no recorded job_id and a live-state task
		// whose recorded job is absent from the hub are the same mismatch:
		// zero active jobs. Both are listed; the client distinguishes them.
		if jobsKnown && liveStates[task.State] && !found {
			mismatch.TasksWithoutJob = append(mismatch.TasksWithoutJob, task.ID)
		}
	}
	sort.Slice(mismatch.TasksWithoutJob, func(i, j int) bool {
		return mismatch.TasksWithoutJob[i] < mismatch.TasksWithoutJob[j]
	})
	if jobsKnown {
		for _, job := range jobs.Items {
			if claimedJobIDs[job.JobID] {
				continue
			}
			if job.OwnerLane != "" && connectedLanes[job.OwnerLane] {
				continue
			}
			mismatch.JobsWithoutTask = append(mismatch.JobsWithoutTask, job.JobID)
		}
		sort.Strings(mismatch.JobsWithoutTask)
	}

	h.writeLive(w, liveResponse{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Jobs:           jobs,
		Nodes:          nodes,
		Links:          links,
		Mismatch:       mismatch,
		TasksTruncated: truncated,
	})
}

func (h *Handler) writeLive(w http.ResponseWriter, response liveResponse) {
	if response.Jobs.Items == nil {
		response.Jobs.Items = []liveJob{}
	}
	if response.Nodes.Items == nil {
		response.Nodes.Items = []liveNode{}
	}
	if response.Links == nil {
		response.Links = []liveTaskLink{}
	}
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "live view unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// liveTasks pages through every task (all lanes and states) so job linkage can
// be attributed to tasks outside the active set as well. The scan is bounded by
// liveTaskScanMax; hitting the bound marks the response truncated.
func (h *Handler) liveTasks(ctx context.Context) ([]store.Task, bool, error) {
	tasks := []store.Task{}
	var after int64
	for len(tasks) < liveTaskScanMax {
		page, err := h.store.ListTasksPage(ctx, "", "", "", after, 1000)
		if err != nil {
			return nil, false, err
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].ID
		tasks = append(tasks, page...)
		if len(page) < 1000 {
			return tasks, false, nil
		}
	}
	return tasks, len(tasks) >= liveTaskScanMax, nil
}

func (p *hubProxy) liveJobsNow(ctx context.Context) liveJobsSection {
	if !p.configured() {
		return liveJobsSection{Status: "unconfigured", Items: []liveJob{}}
	}
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	cached := &p.liveJobsC
	if !cached.at.IsZero() {
		ttl := liveFailCacheTTL
		if cached.last.Status == "ok" {
			ttl = p.cacheTTL
		}
		if time.Since(cached.at) < ttl {
			return cached.last
		}
	}
	jobs, status, err := p.jobs(ctx)
	var section liveJobsSection
	switch {
	case err == nil:
		section = liveJobsSection{
			Status:    "ok",
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
			Items:     projectLiveJobs(jobs),
		}
		copied := section
		copied.Items = append([]liveJob{}, section.Items...)
		cached.ok = &copied
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		section = liveJobsDegraded("unsupported", cached.ok)
	default:
		section = liveJobsDegraded(classifyUpstream(status, err), cached.ok)
	}
	cached.last, cached.at = section, time.Now()
	return section
}

func liveJobsDegraded(status string, ok *liveJobsSection) liveJobsSection {
	section := liveJobsSection{Status: status, Items: []liveJob{}}
	if ok != nil {
		section.FetchedAt = ok.FetchedAt
		section.Items = ok.Items
	}
	return section
}

func (p *hubProxy) liveNodesNow(ctx context.Context) liveNodesSection {
	if !p.configured() {
		return liveNodesSection{Status: "unconfigured", Items: []liveNode{}}
	}
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	cached := &p.liveNodesC
	if !cached.at.IsZero() {
		ttl := liveFailCacheTTL
		if cached.last.Status == "ok" {
			ttl = p.cacheTTL
		}
		if time.Since(cached.at) < ttl {
			return cached.last
		}
	}
	var wrapped struct {
		Nodes []hubNode `json:"nodes"`
	}
	status, err := p.getJSON(ctx, "/v1/nodes", &wrapped)
	var section liveNodesSection
	if err == nil {
		section = liveNodesSection{
			Status:    "ok",
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
			Items:     projectLiveNodes(wrapped.Nodes),
		}
		copied := section
		copied.Items = append([]liveNode{}, section.Items...)
		cached.ok = &copied
	} else {
		section = liveNodesDegraded(classifyUpstream(status, err), cached.ok)
	}
	cached.last, cached.at = section, time.Now()
	return section
}

func liveNodesDegraded(status string, ok *liveNodesSection) liveNodesSection {
	section := liveNodesSection{Status: status, Items: []liveNode{}}
	if ok != nil {
		section.FetchedAt = ok.FetchedAt
		section.Items = ok.Items
	}
	return section
}

func projectLiveJobs(jobs []hubJob) []liveJob {
	out := make([]liveJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, liveJob{
			Machine:       job.Machine,
			JobID:         job.JobID,
			OwnerLane:     job.OwnerLane,
			Pane:          job.Pane,
			Tier:          job.Tier,
			Role:          job.Role,
			StartedAt:     job.StartedAt,
			LastEventKind: job.LastEventKind,
			LastEventAt:   job.LastEventAt,
		})
	}
	return out
}

func projectLiveNodes(nodes []hubNode) []liveNode {
	out := make([]liveNode, 0, len(nodes))
	for _, node := range nodes {
		view := liveNode{
			MachineID:          node.MachineID,
			State:              node.State,
			AcceptingEffective: node.AcceptingEffective,
			LastPingMS:         node.LastPingMS,
			DisplayState:       "missing",
		}
		if node.Load != nil {
			view.Load = &liveLoad{
				Load1:  node.Load.Load1,
				Load5:  node.Load.Load5,
				Load15: node.Load.Load15,
				NCPU:   node.Load.NCPU,
			}
		}
		if node.Memory != nil {
			view.Memory = &liveMemory{
				FreePct:      node.Memory.FreePct,
				CompressedMB: node.Memory.CompressedMB,
				SwapUsedMB:   node.Memory.SwapUsedMB,
				PSISomeAvg10: node.Memory.PSISomeAvg10,
				Source:       node.Memory.Source,
			}
		}
		if node.SessionSnapshot != nil {
			snap := node.SessionSnapshot
			view.SessionSnapshot = &liveSnapshot{
				Sessions:       projectLiveSessions(snap.Sessions),
				SnapshotStatus: snap.SnapshotStatus,
				Truncated:      snap.Truncated,
				ReceivedAt:     snap.ReceivedAt,
				Stale:          snap.Stale,
			}
			view.DisplayState = classifyDisplayState(snap.SnapshotStatus, len(snap.Sessions))
		}
		out = append(out, view)
	}
	return out
}

func projectLiveSessions(sessions []hubSession) []liveSession {
	out := make([]liveSession, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, liveSession{
			PaneID:           session.PaneID,
			WorkspaceID:      session.WorkspaceID,
			Label:            session.Label,
			AgentName:        session.AgentName,
			DisplayLabel:     session.DisplayLabel,
			Status:           session.Status,
			InteractiveReady: session.InteractiveReady,
			Revision:         session.Revision,
			StateChangeSeq:   session.StateChangeSeq,
		})
	}
	return out
}

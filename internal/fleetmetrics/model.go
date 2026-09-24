// Package fleetmetrics computes the five fleet operating metrics of
// hk:doc advice/2026-09-24/fleet-structure-astra Q6 from a read-only
// snapshot of hk tasks, relay events, panewire job records, scopefuel reps,
// deploy records and GitHub PR facts (#644).
//
// The package is pure: Compute takes a Snapshot and returns a Report. Every
// source is collected elsewhere (cmd/handoffkeep fleet-metrics) and can be
// saved and replayed, so the same snapshot always yields the same report.
//
// The one rule every metric follows: a missing identifier or timestamp lowers
// that metric's COVERAGE. It is never turned into a zero duration, zero
// rounds, a "normal path" task or an empty slot.
package fleetmetrics

import (
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// SnapshotSchema versions the saved snapshot JSON.
const SnapshotSchema = "fleet-metrics-snapshot/v1"

// Snapshot is everything Compute reads. Collectors fill it; tests build it
// from fixtures.
type Snapshot struct {
	Schema      string    `json:"schema"`
	CollectedAt time.Time `json:"collected_at"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`

	// Tasks is the full task export. Tasks whose Events is nil were not
	// fetched (unchanged since before the window) and are treated as having
	// held their current state for the whole window.
	Tasks []store.Task `json:"tasks"`
	// TaskComments holds the append-only comment thread per task id, used
	// only for explicit repair/cause tags.
	TaskComments map[int64][]Comment `json:"task_comments,omitempty"`
	// Relay is the hk relay_events stream (job.* and lane.event).
	Relay []store.RelayEvent `json:"relay"`
	// Jobs are panewire/wrk job records reconstructed from job directories.
	Jobs []Job `json:"jobs"`
	// JobRoots names every job-directory root that was read and the machine
	// it belongs to; a machine without a root has no slot history.
	JobRoots []JobRoot `json:"job_roots"`
	// Reps are scopefuel representative-run rows, from every machine supplied.
	Reps []Rep `json:"reps"`
	// RepSources names each reps source read (one per machine).
	RepSources []string `json:"rep_sources"`
	// Deploys are parsed deploy/<service>/<stamp> documents.
	Deploys []DeployRecord `json:"deploys"`
	// PRs holds GitHub facts keyed by canonical PR URL.
	PRs map[string]PRFact `json:"prs"`
	// Contains records GitHub ancestry answers: key "<repo>@<merge>..<deployed>".
	Contains map[string]bool `json:"contains,omitempty"`
	// Slots is the task-slot capacity per machine (builder+tester = 1 task).
	Slots []SlotCapacity `json:"slots"`
	// Notes are collector warnings (a source that failed or was skipped).
	Notes []string `json:"notes,omitempty"`
}

type Comment struct {
	ID   int64     `json:"id"`
	Body string    `json:"body"`
	At   time.Time `json:"created_at"`
}

type JobRoot struct {
	Path    string `json:"path"`
	Machine string `json:"machine"`
}

// JobEvent is one record in a job's events directory. At is the event's own
// timestamp when it carries one; otherwise AtSource says where the time came
// from ("mtime" = the file's modification time) or is empty when unknown.
type JobEvent struct {
	Seq      int        `json:"seq"`
	Kind     string     `json:"kind"`
	At       *time.Time `json:"at,omitempty"`
	AtSource string     `json:"at_source,omitempty"` // "payload" | "mtime" | ""
	Epoch    int        `json:"epoch,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	PR       string     `json:"pr,omitempty"`
	Head     string     `json:"head,omitempty"`
}

// Job is one wrk/panewire job directory.
type Job struct {
	JobID      string `json:"job_id"`
	Machine    string `json:"machine"`
	Role       string `json:"role"` // builder | worker | "" (unknown)
	OwnerLane  string `json:"owner_lane"`
	ParentLane string `json:"parent_lane,omitempty"`
	Tier       string `json:"tier,omitempty"`
	Profile    string `json:"profile,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	// Live is true when the job's pane was present at collection time; a job
	// with no terminal event that is not live has an unknown end.
	Live   bool       `json:"live,omitempty"`
	Events []JobEvent `json:"events"`
	// Status is the completion sentinel's ~30s pane-status samples folded
	// into runs (completion-sentinel.log).
	Status []StatusRun `json:"status,omitempty"`
}

// StatusRun is a maximal run of one normalized sentinel status. Status is
// "working", "idle" (idle/done/blocked: alive, not working), "gone"
// (err:agent_not_found: the pane no longer exists) or "unknown".
type StatusRun struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Status string    `json:"status"`
}

// Rep is one scopefuel reps row.
type Rep struct {
	ID            int64     `json:"id"`
	Source        string    `json:"source"`
	Profile       string    `json:"profile"`
	Task          string    `json:"task"`
	Role          string    `json:"role"`
	Rounds        *int      `json:"rounds,omitempty"`
	BlockersFound *int      `json:"blockers_found,omitempty"`
	Completed     *int      `json:"completed,omitempty"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// DeployRecord is one parsed deploy/<service>/<stamp> document.
type DeployRecord struct {
	Key         string     `json:"key"`
	Service     string     `json:"service"`
	Result      string     `json:"result"` // success | failed | rolled_back | "" (unknown)
	DeployedAt  *time.Time `json:"deployed_at,omitempty"`
	DeployedRef string     `json:"deployed_ref,omitempty"`
	IncludedPRs []string   `json:"included_prs,omitempty"`
	Format      string     `json:"format"` // json | yaml | invalid
}

// PRFact is the GitHub view of one pull request.
type PRFact struct {
	URL       string     `json:"url"`
	Repo      string     `json:"repo"`
	Number    int        `json:"number"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	MergeSHA  string     `json:"merge_sha,omitempty"`
	HeadSHA   string     `json:"head_sha,omitempty"`
	FetchNote string     `json:"fetch_note,omitempty"`
}

// SlotCapacity is a machine's task-slot count from From on (nil = the whole
// window). A machine's capacity at t is its latest entry with From <= t;
// before its first dated entry the capacity is undefined and those minutes
// are left out of the denominator. Source records where the number came from.
type SlotCapacity struct {
	Machine string     `json:"machine"`
	Slots   int        `json:"slots"`
	From    *time.Time `json:"from,omitempty"`
	Source  string     `json:"source"`
}

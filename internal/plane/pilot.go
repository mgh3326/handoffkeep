package plane

import (
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// MirrorPlan is the dry-run evaluation of what the mirror would carry. It is
// computed from a tasks export snapshot — a read-only artifact — so the
// evaluation never touches a live remote or the production database.
type MirrorPlan struct {
	Tasks int `json:"tasks"`
	// Mirrored counts non-terminal tasks — the mirror's selection rule.
	Mirrored int `json:"mirrored"`
	// Terminal counts tasks that would leave the projection (archived under
	// the Linear option, deleted under the Plane option).
	Terminal         int            `json:"terminal"`
	ByState          map[string]int `json:"by_state"`
	ByProject        map[string]int `json:"by_project"`
	UnmappedProjects map[string]int `json:"unmapped_projects"`
	// Unmapped counts non-terminal tasks whose project is neither in the
	// configured map nor covered by a default — they would fail closed.
	Unmapped int `json:"unmapped"`
	// TouchedLast7d counts tasks updated in the last week — an upper bound
	// on transitions per week, since relanes and reclassifies also bump
	// updated_at. Used as the churn estimate in the option comparison.
	TouchedLast7d int `json:"touched_last_7d"`
	// TerminalLast7d counts terminal-state tasks touched in the last week —
	// the archive-churn proxy for the comparison table.
	TerminalLast7d  int  `json:"terminal_last_7d"`
	LinearCap       int  `json:"linear_cap"`
	LinearHeadroom  int  `json:"linear_headroom"`
	LinearFits      bool `json:"linear_fits"`
	SensitiveTitles int  `json:"sensitive_titles"`
	// EstimatedCallsPerDay is the remote write+read volume both options
	// share: ~2 calls per mirrored change plus one daily reconcile read per
	// mirrored task. It is an upper bound — label/state lookups amortize.
	EstimatedCallsPerDay float64 `json:"estimated_calls_per_day"`
}

// callsPerUpdate is the per-op remote cost of the writer: one adoption or
// link read plus the write itself; state/label/project lookups amortize
// inside the drain.
const callsPerUpdate = 2.0

// EvaluateMirror computes the pilot numbers for both options from one
// export snapshot. now anchors the 7-day windows; mapping resolves which
// non-terminal tasks would fail closed on an unmapped project.
func EvaluateMirror(tasks []store.Task, now time.Time, mapping ProjectMapping, linearCap int) MirrorPlan {
	plan := MirrorPlan{
		ByState:          map[string]int{},
		ByProject:        map[string]int{},
		UnmappedProjects: map[string]int{},
		LinearCap:        linearCap,
	}
	week := now.Add(-7 * 24 * time.Hour)
	for _, task := range tasks {
		plan.Tasks++
		plan.ByState[task.State]++
		terminal := store.PlaneTaskTerminal(task)
		if terminal {
			plan.Terminal++
		} else {
			plan.Mirrored++
			project := ""
			if task.Project != nil {
				project = *task.Project
			}
			bucket := project
			if bucket == "" {
				bucket = "(unset)"
			}
			plan.ByProject[bucket]++
			if _, err := mapping.IdentifierFor(project); err != nil {
				plan.Unmapped++
				plan.UnmappedProjects[bucket]++
			}
			if store.PlaneTitleIsSensitive(task.Title) {
				plan.SensitiveTitles++
			}
		}
		if task.UpdatedAt.After(week) {
			plan.TouchedLast7d++
			if terminal {
				plan.TerminalLast7d++
			}
		}
	}
	plan.LinearHeadroom = linearCap - plan.Mirrored
	plan.LinearFits = plan.LinearHeadroom >= 0
	// Writer cost: one op per task write (bounded by touched count) plus the
	// daily reconcile read pass over the mirrored set.
	dailyWrites := float64(plan.TouchedLast7d) / 7.0
	plan.EstimatedCallsPerDay = dailyWrites*callsPerUpdate + float64(plan.Mirrored)
	return plan
}

// SortedUnmappedProjects renders the unmapped buckets deterministically for
// reports.
func (plan MirrorPlan) SortedUnmappedProjects() []string {
	names := make([]string, 0, len(plan.UnmappedProjects))
	for name := range plan.UnmappedProjects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

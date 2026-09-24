package fleetmetrics

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Link tiers, strongest first. A structured tier comes from a typed field;
// the others are derived from text or naming convention and are reported
// separately in coverage so a reader can discount them.
const (
	LinkRefs       = "refs"        // refs.job_id / refs.pr on the task or a task event
	LinkRelay      = "relay"       // PR carried by a linked job's relay/job event
	LinkReportPath = "report_path" // refs.report_path under jobs/<job_id>/
	LinkNote       = "note"        // exact known job id or PR URL inside a task event note
	LinkName       = "name"        // job id named "<task>-…", "<task>b-…" or "t<task>-…"
)

var (
	prURLRE      = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)`)
	tokenRE      = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.-]{4,}`)
	jobNameRE    = regexp.MustCompile(`^t?([0-9]+)[a-z]?-`)
	reportPathRE = regexp.MustCompile(`/jobs/([^/]+)/[^/]*$`)
)

// CanonicalPR returns "https://github.com/o/r/pull/n" and the repo "o/r",
// or empty strings when s holds no PR URL.
func CanonicalPR(s string) (url, repo string, number int) {
	m := prURLRE.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0
	}
	n, _ := strconv.Atoi(m[3])
	return "https://github.com/" + m[1] + "/" + m[2] + "/pull/" + m[3], m[1] + "/" + m[2], n
}

type index struct {
	snap      *Snapshot
	tasks     map[int64]*store.Task
	jobs      map[string]*Job
	relayJobs map[string][]store.RelayEvent
	knownJobs map[string]bool
	nameJobs  map[string][]string         // task id prefix → job ids named "<id>-…"
	taskJobs  map[int64]map[string]string // task → job id → tier
	taskPRs   map[int64]map[string]string // task → PR URL → tier
	lineage   map[int64]int64             // task → lineage root
}

func buildIndex(s *Snapshot) *index {
	ix := &index{
		snap:      s,
		tasks:     map[int64]*store.Task{},
		jobs:      map[string]*Job{},
		relayJobs: map[string][]store.RelayEvent{},
		knownJobs: map[string]bool{},
		nameJobs:  map[string][]string{},
		taskJobs:  map[int64]map[string]string{},
		taskPRs:   map[int64]map[string]string{},
		lineage:   map[int64]int64{},
	}
	for i := range s.Tasks {
		ix.tasks[s.Tasks[i].ID] = &s.Tasks[i]
	}
	for i := range s.Jobs {
		ix.jobs[s.Jobs[i].JobID] = &s.Jobs[i]
		ix.knownJobs[s.Jobs[i].JobID] = true
	}
	for _, e := range s.Relay {
		if e.JobID != "" {
			ix.relayJobs[e.JobID] = append(ix.relayJobs[e.JobID], e)
			ix.knownJobs[e.JobID] = true
		}
	}
	for job := range ix.knownJobs {
		if m := jobNameRE.FindStringSubmatch(job); m != nil {
			ix.nameJobs[m[1]] = append(ix.nameJobs[m[1]], job)
		}
	}
	for id, t := range ix.tasks {
		ix.taskJobs[id] = ix.linkJobs(t)
		ix.taskPRs[id] = ix.linkPRs(t, ix.taskJobs[id])
	}
	ix.buildLineage()
	return ix
}

func better(tiers map[string]string, key, tier string) {
	order := map[string]int{LinkRefs: 0, LinkRelay: 1, LinkReportPath: 2, LinkNote: 3, LinkName: 4}
	if old, ok := tiers[key]; !ok || order[tier] < order[old] {
		tiers[key] = tier
	}
}

func allRefs(t *store.Task) []store.TaskRefs {
	out := []store.TaskRefs{t.Refs}
	for _, e := range t.Events {
		if e.Refs != nil {
			out = append(out, *e.Refs)
		}
	}
	return out
}

func (ix *index) linkJobs(t *store.Task) map[string]string {
	out := map[string]string{}
	for _, r := range allRefs(t) {
		if r.JobID != "" {
			better(out, r.JobID, LinkRefs)
		}
		if m := reportPathRE.FindStringSubmatch(r.ReportPath); m != nil && ix.knownJobs[m[1]] {
			better(out, m[1], LinkReportPath)
		}
	}
	for _, e := range t.Events {
		for _, tok := range tokenRE.FindAllString(e.Note, -1) {
			if ix.knownJobs[tok] {
				better(out, tok, LinkNote)
			}
		}
	}
	for _, job := range ix.nameJobs[strconv.FormatInt(t.ID, 10)] {
		better(out, job, LinkName)
	}
	return out
}

func (ix *index) linkPRs(t *store.Task, jobs map[string]string) map[string]string {
	out := map[string]string{}
	for _, r := range allRefs(t) {
		if u, _, _ := CanonicalPR(r.PR); u != "" {
			better(out, u, LinkRefs)
		}
	}
	for job := range jobs {
		for _, e := range ix.relayJobs[job] {
			if u, _, _ := CanonicalPR(e.PR); u != "" {
				better(out, u, LinkRelay)
			}
		}
		if j := ix.jobs[job]; j != nil {
			for _, e := range j.Events {
				if u, _, _ := CanonicalPR(e.PR); u != "" {
					better(out, u, LinkRelay)
				}
			}
		}
	}
	for _, e := range t.Events {
		for _, m := range prURLRE.FindAllString(e.Note, -1) {
			u, _, _ := CanonicalPR(m)
			better(out, u, LinkNote)
		}
	}
	return out
}

func (ix *index) buildLineage() {
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		return x
	}
	union := func(a, b int64) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	for id, t := range ix.tasks {
		parent[id] = find(id)
		for _, r := range allRefs(t) {
			if r.OriginTask > 0 {
				if _, ok := ix.tasks[r.OriginTask]; ok {
					union(id, r.OriginTask)
				}
			}
		}
	}
	for id := range ix.tasks {
		ix.lineage[id] = find(id)
	}
}

// stateEvent reports whether e is a state transition. relane rows carry lane
// names and decision rows (#618) record a request or its resolution with
// from == to == the current state; neither enters or leaves a state, so
// every state reader here skips them. Pre-kind rows ("") are transitions.
func stateEvent(e store.TaskEvent) bool {
	return e.Kind == "" || e.Kind == store.TaskEventTransition
}

// firstTo returns the first transition event into state (at or after
// notBefore when non-zero).
func firstTo(t *store.Task, state string) *store.TaskEvent {
	for i := range t.Events {
		e := &t.Events[i]
		if stateEvent(*e) && e.To == state {
			return e
		}
	}
	return nil
}

func lastTo(t *store.Task, state string) *store.TaskEvent {
	var out *store.TaskEvent
	for i := range t.Events {
		e := &t.Events[i]
		if stateEvent(*e) && e.To == state {
			out = e
		}
	}
	return out
}

// terminalAt is when the task reached its terminal state, or nil.
func terminalAt(t *store.Task) *time.Time {
	if t.State != "merged" && t.State != "dropped" {
		return nil
	}
	if e := lastTo(t, t.State); e != nil {
		at := e.At
		return &at
	}
	if t.Events == nil {
		// Not refetched: the task has not changed since UpdatedAt, which is
		// therefore when it reached its terminal state.
		at := t.UpdatedAt
		return &at
	}
	return nil
}

// stateAt replays a task's transitions to its state at time at. A task whose
// events were not fetched (Events == nil) is assumed to have held its current
// state since UpdatedAt; before CreatedAt it does not exist ("").
func stateAt(t *store.Task, at time.Time) string {
	if at.Before(t.CreatedAt) {
		return ""
	}
	if t.Events == nil {
		if !at.Before(t.UpdatedAt) {
			return t.State
		}
		return "?"
	}
	state := "backlog"
	for _, e := range t.Events {
		if !stateEvent(e) {
			continue
		}
		if e.At.After(at) {
			break
		}
		state = e.To
	}
	return state
}

func dist(values []float64) *Dist {
	if len(values) == 0 {
		return nil
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	rank := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(v)))) - 1
		if i < 0 {
			i = 0
		}
		return round2(v[i])
	}
	return &Dist{N: len(v), P50: rank(0.5), P90: rank(0.9), Max: round2(v[len(v)-1])}
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func hours(d time.Duration) float64 { return d.Hours() }

func inWindow(at time.Time, s *Snapshot) bool {
	return !at.Before(s.Since) && at.Before(s.Until)
}

func bestTier(tiers map[string]string) string {
	best := ""
	order := map[string]int{LinkRefs: 0, LinkRelay: 1, LinkReportPath: 2, LinkNote: 3, LinkName: 4}
	for _, t := range tiers {
		if best == "" || order[t] < order[best] {
			best = t
		}
	}
	return best
}

func hasTag(text, tag string) bool {
	return strings.Contains(strings.ToLower(text), tag)
}

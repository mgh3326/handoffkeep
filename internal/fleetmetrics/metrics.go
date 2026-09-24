package fleetmetrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// DeployRepos maps deploy-record services onto the GitHub repo whose merges
// they ship. A repo listed here is in the "deploy" lead-time group; any other
// repo has no deploy-record stream and is in the "merge-only" group.
var DeployRepos = deployRepoMap([]struct{ Service, Repo string }{
	{Service: "handoffkeep", Repo: "mgh3326/handoffkeep"},
	{Service: "auto_trader", Repo: "mgh3326/auto_trader"},
	{Service: "panewire-hub", Repo: "mgh3326/panewire"},
	{Service: "panewire-node", Repo: "mgh3326/panewire"},
	{Service: "scopefuel", Repo: "mgh3326/scopefuel"},
	{Service: "brewdial-api", Repo: "mgh3326/brewdial"},
	{Service: "brewdial-db", Repo: "mgh3326/brewdial"},
	{Service: "brewdial-data", Repo: "mgh3326/brewdial"},
})

func deployRepoMap(xs []struct{ Service, Repo string }) map[string]string {
	out := make(map[string]string, len(xs))
	for _, x := range xs {
		out[x.Service] = x.Repo
	}
	return out
}

// Compute derives the report from a snapshot. It never mutates s.
func Compute(s Snapshot) Report {
	ix := buildIndex(&s)
	r := Report{Since: s.Since, Until: s.Until, CollectedAt: s.CollectedAt, Notes: append([]string(nil), s.Notes...)}
	r.LeadTime = leadTime(ix)
	r.Rounds = rounds(ix)
	r.EmptySlots = slots(ix)
	r.Decisions = decisions(ix)
	r.NormalPath = normalPath(ix)
	r.Sources = sources(&s)
	return r
}

func sources(s *Snapshot) []string {
	out := []string{
		fmt.Sprintf("hk tasks: %d rows (%d with event history)", len(s.Tasks), countEvents(s.Tasks)),
		fmt.Sprintf("hk relay events: %d", len(s.Relay)),
		fmt.Sprintf("job directories: %d jobs from %d root(s)", len(s.Jobs), len(s.JobRoots)),
		fmt.Sprintf("scopefuel reps: %d rows from %d source(s)", len(s.Reps), len(s.RepSources)),
		fmt.Sprintf("deploy records: %d", len(s.Deploys)),
		fmt.Sprintf("GitHub PR facts: %d", len(s.PRs)),
	}
	for _, root := range s.JobRoots {
		out = append(out, "job root "+root.Machine+": "+root.Path)
	}
	for _, src := range s.RepSources {
		out = append(out, "reps source: "+src)
	}
	return out
}

func countEvents(ts []store.Task) int {
	n := 0
	for _, t := range ts {
		if t.Events != nil {
			n++
		}
	}
	return n
}

func sortedIDs(ix *index) []int64 {
	ids := make([]int64, 0, len(ix.tasks))
	for id := range ix.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ---------------------------------------------------------------- metric 1

type prUsable struct {
	group    string // deploy | merge_only
	mergedAt *time.Time
	usableAt *time.Time
	state    string // usable | awaiting_deploy | deploy_link_unknown | merge_unknown
	mergeSrc string // github | hk
}

func (ix *index) prUsable(url, repo string, number int, hkMerged *time.Time) prUsable {
	out := prUsable{group: "merge_only"}
	fact, ok := ix.snap.PRs[url]
	if ok && fact.MergedAt != nil {
		out.mergedAt, out.mergeSrc = fact.MergedAt, "github"
	} else if hkMerged != nil {
		out.mergedAt, out.mergeSrc = hkMerged, "hk"
	}
	if out.mergedAt == nil {
		out.state = "merge_unknown"
		return out
	}
	services := []string{}
	for svc, r := range DeployRepos {
		if r == repo {
			services = append(services, svc)
		}
	}
	if len(services) == 0 {
		out.state, out.usableAt = "usable", out.mergedAt
		return out
	}
	out.group = "deploy"
	// A merge older than the service's first deploy record cannot be linked:
	// deploys before the record stream began were not written down.
	var firstRecord *time.Time
	for _, d := range ix.snap.Deploys {
		if containsString(services, d.Service) && d.DeployedAt != nil && (firstRecord == nil || d.DeployedAt.Before(*firstRecord)) {
			firstRecord = d.DeployedAt
		}
	}
	// No dated record at all means the stream exists but none of it parsed
	// (or carries a deployed_at); that is not evidence the merge is older.
	if firstRecord == nil {
		out.state = "no dated deploy record for the repo's services"
		return out
	}
	if out.mergedAt.Before(*firstRecord) {
		out.state = "merged before the deploy-record stream began"
		return out
	}
	short := repo[strings.Index(repo, "/")+1:] + "#" + strconv.Itoa(number)
	var best *time.Time
	undecided := false
	for _, d := range ix.snap.Deploys {
		if d.Result != "success" || d.DeployedAt == nil || d.DeployedAt.Before(*out.mergedAt) {
			continue
		}
		if !containsString(services, d.Service) {
			continue
		}
		included := false
		for _, p := range d.IncludedPRs {
			if strings.Contains(p, url) || strings.HasPrefix(strings.TrimSpace(p), short) && !isDigitAfter(strings.TrimSpace(p), len(short)) {
				included = true
			}
		}
		if !included && fact.MergeSHA != "" && d.DeployedRef != "" {
			ref := strings.TrimPrefix(d.DeployedRef, "pw-")
			if len(ref) >= 7 && strings.HasPrefix(fact.MergeSHA, ref) || len(ref) >= 7 && strings.HasPrefix(ref, fact.MergeSHA) {
				included = true
			} else if v, ok := ix.snap.Contains[repo+"@"+fact.MergeSHA+".."+ref]; ok {
				included = v
			} else if len(d.IncludedPRs) == 0 {
				undecided = true
			}
		} else if !included && len(d.IncludedPRs) == 0 {
			undecided = true
		}
		if included && (best == nil || d.DeployedAt.Before(*best)) {
			at := *d.DeployedAt
			best = &at
		}
	}
	switch {
	case best != nil:
		out.state, out.usableAt = "usable", best
	case undecided:
		out.state = "deploy_link_unknown"
	default:
		out.state = "awaiting_deploy"
	}
	return out
}

func isDigitAfter(s string, i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func leadTime(ix *index) LeadTime {
	out := LeadTime{Phases: map[string]*Dist{}}
	var deploy, mergeOnly, open []float64
	phases := map[string][]float64{}
	pop := Coverage{What: "tasks merged in window with order time, PR and usable time observed", Detail: map[string]int{}}
	prLink := Coverage{What: "merged tasks with a linked PR", Detail: map[string]int{}}
	mergeSrc := Coverage{What: "linked PRs with a GitHub merge time", Detail: map[string]int{}}
	deployLink := Coverage{What: "deploy-group PRs with a linked successful deploy record", Detail: map[string]int{}}
	for _, id := range sortedIDs(ix) {
		t := ix.tasks[id]
		term := terminalAt(t)
		if t.State == "merged" && term != nil && inWindow(*term, ix.snap) {
			out.Completed++
			pop.Total++
			prLink.Total++
			order := firstTo(t, "claimed")
			prs := ix.taskPRs[id]
			if len(prs) > 0 {
				prLink.Linked++
				prLink.Detail[bestTier(prs)]++
			} else {
				prLink.Detail["no PR link"]++
			}
			if order == nil {
				pop.Detail["no claim event"]++
				continue
			}
			if len(prs) == 0 {
				pop.Detail["no PR link"]++
				continue
			}
			var usable *time.Time
			var merged *time.Time
			group := "merge_only"
			state := "usable"
			urls := make([]string, 0, len(prs))
			for u := range prs {
				urls = append(urls, u)
			}
			sort.Strings(urls)
			for _, u := range urls {
				_, repo, n := CanonicalPR(u)
				pu := ix.prUsable(u, repo, n, term)
				mergeSrc.Total++
				if pu.mergeSrc == "github" {
					mergeSrc.Linked++
				} else {
					mergeSrc.Detail["merge time from hk transition"]++
				}
				if pu.group == "deploy" {
					group = "deploy"
					deployLink.Total++
					if pu.state == "usable" {
						deployLink.Linked++
					} else {
						deployLink.Detail[pu.state]++
					}
				}
				if pu.state != "usable" {
					if state == "usable" || state == "awaiting_deploy" {
						state = pu.state
					}
					continue
				}
				if usable == nil || pu.usableAt.After(*usable) {
					usable = pu.usableAt
				}
				if merged == nil || pu.mergedAt.After(*merged) {
					merged = pu.mergedAt
				}
			}
			if state != "usable" {
				pop.Detail[state]++
				continue
			}
			d := usable.Sub(order.At)
			if d < 0 {
				pop.Detail["usable before order (inconsistent)"]++
				continue
			}
			pop.Linked++
			if group == "deploy" {
				deploy = append(deploy, hours(d))
			} else {
				mergeOnly = append(mergeOnly, hours(d))
			}
			if v := firstTo(t, "verifying"); v != nil && !v.At.Before(order.At) && !merged.Before(v.At) {
				phases["implement"] = append(phases["implement"], hours(v.At.Sub(order.At)))
				phases["verify_fix"] = append(phases["verify_fix"], hours(merged.Sub(v.At)))
			}
			phases["decision_wait"] = append(phases["decision_wait"], hours(timeIn(t, "needs_decision", order.At, *merged)))
			if group == "deploy" {
				phases["install"] = append(phases["install"], hours(usable.Sub(*merged)))
			}
		}
		if t.State != "merged" && t.State != "dropped" && t.State != "backlog" {
			if order := firstTo(t, "claimed"); order != nil {
				out.OpenCount++
				open = append(open, hours(ix.snap.Until.Sub(order.At)))
			} else {
				out.OpenUnordered++
			}
		}
	}
	out.Deploy, out.MergeOnly, out.OpenAge = dist(deploy), dist(mergeOnly), dist(open)
	for k, v := range phases {
		out.Phases[k] = dist(v)
	}
	openCov := Coverage{What: "open (claimed…hold) tasks with an observed claim time", Linked: out.OpenCount, Total: out.OpenCount + out.OpenUnordered}
	out.Coverage = []Coverage{pop, prLink, mergeSrc, deployLink, openCov}
	return out
}

// timeIn sums the time a task spent in state inside [from, to).
func timeIn(t *store.Task, state string, from, to time.Time) time.Duration {
	var total time.Duration
	var enter *time.Time
	for _, e := range t.Events {
		if !stateEvent(e) {
			continue
		}
		if e.To == state && enter == nil {
			at := e.At
			enter = &at
		} else if e.From == state && e.To != state && enter != nil {
			total += overlap(*enter, e.At, from, to)
			enter = nil
		}
	}
	if enter != nil {
		total += overlap(*enter, to, from, to)
	}
	return total
}

func overlap(a0, a1, b0, b1 time.Time) time.Duration {
	lo, hi := a0, a1
	if b0.After(lo) {
		lo = b0
	}
	if b1.Before(hi) {
		hi = b1
	}
	if hi.After(lo) {
		return hi.Sub(lo)
	}
	return 0
}

// ---------------------------------------------------------------- metric 2

// CauseTags are the explicit cause markers read from fix-round notes.
var CauseTags = map[string]string{"cause:regression": "regression", "cause=regression": "regression", "cause:requirement": "requirement", "cause=requirement": "requirement"}

func rounds(ix *index) Rounds {
	out := Rounds{Cause: map[string]int{}}
	members := map[int64][]int64{}
	for id, root := range ix.lineage {
		members[root] = append(members[root], id)
	}
	roots := make([]int64, 0, len(members))
	for root, ids := range members {
		for _, id := range ids {
			if at := terminalAt(ix.tasks[id]); at != nil && inWindow(*at, ix.snap) {
				roots = append(roots, root)
				break
			}
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	cov := Coverage{What: "lineages (task + origin_task chain) terminal in window with ≥1 verify transition", Total: len(roots), Detail: map[string]int{}}
	causeCov := Coverage{What: "fix rounds (verifying→in_progress) with an explicit cause tag", Detail: map[string]int{}}
	repsCov := Coverage{What: "lineages with a scopefuel impl rep carrying rounds", Total: len(roots), Detail: map[string]int{}}
	latestRep := map[string]Rep{}
	for _, rep := range ix.snap.Reps {
		if rep.Role != "impl" || rep.Rounds == nil {
			continue
		}
		if old, ok := latestRep[rep.Task]; !ok || rep.RecordedAt.After(old.RecordedAt) {
			latestRep[rep.Task] = rep
		}
	}
	var perLineage, repsRounds []float64
	for _, root := range roots {
		n := 0
		repN, repSeen := 0, false
		for _, id := range members[root] {
			t := ix.tasks[id]
			for _, e := range t.Events {
				if !stateEvent(e) {
					continue
				}
				if e.To == "verifying" {
					n++
				}
				if e.From == "verifying" && e.To == "in_progress" {
					out.FixRounds++
					causeCov.Total++
					tagged := ""
					for tag, cause := range CauseTags {
						if hasTag(e.Note, tag) {
							tagged = cause
						}
					}
					if tagged != "" {
						causeCov.Linked++
						out.Cause[tagged]++
					}
				}
			}
			if rep, ok := latestRep[strconv.FormatInt(id, 10)]; ok {
				repN += *rep.Rounds
				repSeen = true
			}
		}
		if repSeen {
			repsCov.Linked++
			repsRounds = append(repsRounds, float64(repN))
		}
		if n == 0 {
			// No verify transition was recorded for this lineage: its round
			// count is unknown, not zero.
			cov.Detail["no verify transition recorded"]++
			continue
		}
		cov.Linked++
		out.Lineages++
		perLineage = append(perLineage, float64(n))
		if n > 3 {
			out.OverThree++
		}
		if repSeen {
			if repN == n {
				out.RepsAgree++
			} else {
				out.RepsDisagree++
			}
		}
	}
	causeCov.Detail["untagged"] = causeCov.Total - causeCov.Linked
	repsCov.Detail["rep sources (machines) read"] = len(ix.snap.RepSources)
	out.PerLineage, out.RepsRounds = dist(perLineage), dist(repsRounds)
	out.Coverage = []Coverage{cov, causeCov, repsCov}
	return out
}

// ---------------------------------------------------------------- metric 3

type interval struct{ from, to time.Time }

// unitSpan is one task unit on a machine: a builder job plus the worker/
// tester jobs it owns (#607: builder+tester = 1 task slot).
type unitSpan struct {
	job *Job
	// occupied is when the unit holds its slot: builder claim → the first of
	// terminal job event, quota release, pane gone, linked task terminal, or
	// collection (live).
	occupied interval
	// unknownFrom is set when the end could not be observed: from here on
	// the unit may or may not hold the slot.
	unknownFrom *time.Time
	workers     []*Job
}

func statusAt(runs []StatusRun, t time.Time) string {
	i := sort.Search(len(runs), func(i int) bool { return !runs[i].To.Before(t) })
	if i < len(runs) && !t.Before(runs[i].From) {
		return runs[i].Status
	}
	return ""
}

// eventState classifies a builder minute from job events alone: after it
// reported (joined/completed) or while an escalation is unanswered, the
// builder is waiting on an external condition.
func eventState(j *Job, t time.Time) string {
	state := "working"
	for _, e := range j.Events {
		if e.At == nil || e.At.After(t) {
			break
		}
		switch e.Kind {
		case "job.joined", "job.completed", "job.escalate":
			state = "idle"
		case "job.reclaim", "job.spawned":
			state = "working"
		}
	}
	return state
}

// stateAt is the unit's slot state at t: "working" when the builder or any of
// its workers is WORKING, "held" when the builder is alive but not working
// (waiting on its tester, CI, an answer or a merge), "" when the unit does
// not hold the slot, "unknown" when it may or may not.
func (u unitSpan) stateAt(t time.Time) string {
	if t.Before(u.occupied.from) {
		return ""
	}
	if u.unknownFrom != nil && !t.Before(*u.unknownFrom) {
		return "unknown"
	}
	if !t.Before(u.occupied.to) {
		return ""
	}
	for _, w := range u.workers {
		if statusAt(w.Status, t) == "working" {
			return "working"
		}
	}
	switch statusAt(u.job.Status, t) {
	case "working":
		return "working"
	case "idle":
		return "held"
	}
	if eventState(u.job, t) == "working" {
		return "working"
	}
	return "held"
}

// unitFor builds a builder's slot interval. taskEnd is the time every task
// linked to this builder reached merged/dropped (nil if any is open or none
// is linked): once its task is terminal a lingering pane holds no slot.
func unitFor(j *Job, taskEnd *time.Time, until time.Time) (unitSpan, string) {
	u := unitSpan{job: j}
	var start, end, last *time.Time
	pick := func(t *time.Time) {
		if t != nil && (end == nil || t.Before(*end)) {
			v := *t
			end = &v
		}
	}
	for _, e := range j.Events {
		if e.At == nil {
			continue
		}
		if e.Kind == "job.claim" && start == nil {
			start = e.At
		}
		if last == nil || e.At.After(*last) {
			last = e.At
		}
		switch e.Kind {
		case "job.reaped", "job.lost", "job.revoked", "quota_pool.release":
			pick(e.At)
		}
	}
	if start == nil {
		return u, "no claim time"
	}
	for _, r := range j.Status {
		if r.Status == "gone" {
			at := r.From
			pick(&at)
			break
		}
		if last == nil || r.To.After(*last) {
			to := r.To
			last = &to
		}
	}
	pick(taskEnd)
	if j.Live {
		pick(&until)
	}
	if end == nil {
		u.occupied = interval{*start, until}
		u.unknownFrom = last
		return u, "end unobserved (no terminal record, pane not live, task open or unlinked)"
	}
	if end.Before(*start) {
		end = start
	}
	u.occupied = interval{*start, *end}
	return u, ""
}

// readyMinutes counts, per window minute, tasks in backlog (-1 = unknown).
func readyMinutes(ix *index, start time.Time, minutes int) []int {
	out := make([]int, minutes)
	unknown := make([]bool, minutes)
	for _, t := range ix.tasks {
		for m := 0; m < minutes; m++ {
			switch stateAt(t, start.Add(time.Duration(m)*time.Minute)) {
			case "backlog":
				out[m]++
			case "?":
				unknown[m] = true
			}
		}
	}
	for m := range out {
		if out[m] == 0 && unknown[m] {
			out[m] = -1
		}
	}
	return out
}

func (ix *index) jobTaskEnd(job string) *time.Time {
	var end *time.Time
	linked := false
	for id, jobs := range ix.taskJobs {
		if _, ok := jobs[job]; !ok {
			continue
		}
		linked = true
		at := terminalAt(ix.tasks[id])
		if at == nil {
			return nil
		}
		if end == nil || at.After(*end) {
			end = at
		}
	}
	if !linked {
		return nil
	}
	return end
}

func slots(ix *index) Slots {
	out := Slots{ByReason: map[string]int{}}
	start := ix.snap.Since.Truncate(time.Minute)
	minutes := int(ix.snap.Until.Sub(start) / time.Minute)
	if minutes < 0 {
		minutes = 0
	}
	rootMachines := map[string]bool{}
	for _, root := range ix.snap.JobRoots {
		rootMachines[root.Machine] = true
	}
	machineCov := Coverage{What: "machines with task slots that have job history (job root read)", Detail: map[string]int{}}
	unitCov := Coverage{What: "builder units overlapping the window with an observed slot end", Detail: map[string]int{}}
	minuteCov := Coverage{What: "machine-minutes where every unit's slot state is known", Detail: map[string]int{}}
	sentinelCov := Coverage{What: "occupied unit-minutes whose working/idle split comes from sentinel samples", Detail: map[string]int{}}
	eligCov := Coverage{What: "empty-with-ready minutes backed by an eligibility snapshot (quota/account/profile)", Detail: map[string]int{}}
	ready := readyMinutes(ix, start, minutes)
	capsBy := map[string][]SlotCapacity{}
	for _, c := range ix.snap.Slots {
		capsBy[c.Machine] = append(capsBy[c.Machine], c)
	}
	machines := make([]string, 0, len(capsBy))
	for m := range capsBy {
		machines = append(machines, m)
	}
	sort.Strings(machines)
	capacityCov := Coverage{What: "machine-minutes with a defined slot capacity", Detail: map[string]int{}}
	for _, machine := range machines {
		timeline := capsBy[machine]
		sort.SliceStable(timeline, func(i, j int) bool {
			if timeline[i].From == nil || timeline[j].From == nil {
				return timeline[i].From == nil && timeline[j].From != nil
			}
			return timeline[i].From.Before(*timeline[j].From)
		})
		capAt := func(t time.Time) int {
			n := -1
			for _, c := range timeline {
				if c.From == nil || !t.Before(*c.From) {
					n = c.Slots
				}
			}
			return n
		}
		c := SlotCapacity{Machine: machine, Slots: timeline[len(timeline)-1].Slots}
		machineCov.Total++
		if !rootMachines[c.Machine] {
			machineCov.Detail["no job history: "+c.Machine]++
			continue
		}
		machineCov.Linked++
		ms := MachineSlots{Machine: c.Machine, Slots: c.Slots}
		var units []unitSpan
		byLane := map[string]int{}
		for i := range ix.snap.Jobs {
			j := &ix.snap.Jobs[i]
			if j.Machine != c.Machine || j.Role != "builder" {
				continue
			}
			u, why := unitFor(j, ix.jobTaskEnd(j.JobID), ix.snap.Until)
			if why == "no claim time" {
				continue
			}
			if !u.occupied.from.Before(ix.snap.Until) || !u.occupied.to.After(ix.snap.Since) {
				continue
			}
			if u.unknownFrom != nil && !u.unknownFrom.Before(ix.snap.Until) {
				u.unknownFrom = nil
			}
			unitCov.Total++
			if why != "" {
				unitCov.Detail[why+": "+j.JobID]++
			} else {
				unitCov.Linked++
			}
			byLane[j.OwnerLane] = len(units)
			units = append(units, u)
		}
		for i := range ix.snap.Jobs {
			w := &ix.snap.Jobs[i]
			if w.Role != "worker" || w.Machine != c.Machine {
				continue
			}
			if k, ok := byLane[w.OwnerLane]; ok && w.OwnerLane != "" {
				units[k].workers = append(units[k].workers, w)
			}
		}
		for m := 0; m < minutes; m++ {
			at := start.Add(time.Duration(m) * time.Minute)
			capacityCov.Total++
			slotsNow := capAt(at)
			if slotsNow < 0 {
				capacityCov.Detail["no capacity declared yet: "+c.Machine]++
				continue
			}
			capacityCov.Linked++
			c.Slots = slotsNow
			minuteCov.Total++
			working, held, unknown := 0, 0, 0
			for _, u := range units {
				switch u.stateAt(at) {
				case "working":
					working++
				case "held":
					held++
				case "unknown":
					unknown++
				}
				if st := u.stateAt(at); st == "working" || st == "held" {
					sentinelCov.Total++
					if statusAt(u.job.Status, at) != "" {
						sentinelCov.Linked++
					}
				}
			}
			w := min(working, c.Slots)
			h := min(held, c.Slots-w)
			empty := c.Slots - w - h
			ms.SlotMinutes += c.Slots
			ms.Working += w
			ms.HeldIdle += h
			out.Overflow += working + held - w - h
			// A unit with an unobserved end may or may not hold a slot here:
			// the minute's empty count is only bounded, [empty-unknown, empty].
			lo := empty
			if unknown > 0 {
				minuteCov.Detail["unit with unobserved end"]++
				lo = max(0, empty-unknown)
				ms.Unobserved += empty - lo
			} else {
				minuteCov.Linked++
			}
			ms.Empty += lo
			if empty == 0 {
				continue
			}
			switch {
			case ready[m] > 0:
				out.ByReason["ready work, eligibility unrecorded"] += lo
				if empty > lo {
					out.ByReason["ready work, slot state unobserved (upper bound only)"] += empty - lo
				}
				ms.EmptyWithReady += lo
				ms.EmptyWithReadyMax += empty
			case ready[m] == 0:
				out.ByReason["no ready work (backlog empty)"] += lo
			default:
				out.ByReason["ready work unknown"] += lo
			}
		}
		out.SlotMinutes += ms.SlotMinutes
		out.Working += ms.Working
		out.HeldIdle += ms.HeldIdle
		out.Empty += ms.Empty
		out.EmptyWithReady += ms.EmptyWithReady
		out.EmptyWithReadyMax += ms.EmptyWithReadyMax
		out.Unobserved += ms.Unobserved
		out.PerMachine = append(out.PerMachine, ms)
	}
	sentinelCov.Detail["split inferred from job events"] = sentinelCov.Total - sentinelCov.Linked
	eligCov.Total = out.EmptyWithReady
	eligCov.Detail["no source records per-minute eligibility"] = out.EmptyWithReady
	out.Coverage = []Coverage{machineCov, capacityCov, unitCov, minuteCov, sentinelCov, eligCov}
	return out
}

// ---------------------------------------------------------------- metric 4

type decisionReq struct {
	kind, ref          string
	at                 time.Time
	answer             *time.Time
	answeredBy         string
	deliver, consume   *time.Time
	consumeUnavailable bool
}

// decisionAnswerEvent finds the answer lane event for a request, using the
// same identity the console's open-decision queries use: task and escalation
// answers carry "decision-<kind>-<id>-" in their event_id (task ids and relay
// ids share a number space, so text alone is ambiguous); a lane decision is
// answered by "[decision-answered] #<id>:" on the same owner lane.
func decisionAnswerEvent(ix *index, kind string, id int64, lane string, after time.Time) *store.RelayEvent {
	needle := "decision-" + kind + "-" + strconv.FormatInt(id, 10) + "-"
	answered := "[decision-answered] #" + strconv.FormatInt(id, 10) + ":"
	var best *store.RelayEvent
	for i := range ix.snap.Relay {
		e := &ix.snap.Relay[i]
		match := strings.Contains(e.EventID, needle)
		if kind == "lane" {
			match = e.OwnerLane == lane && strings.HasPrefix(e.Text, answered)
		}
		if e.Kind == "lane.event" && match && !e.ReceivedAt.Before(after) {
			if best == nil || e.ReceivedAt.Before(best.ReceivedAt) {
				best = e
			}
		}
	}
	return best
}

func decisions(ix *index) Decisions {
	out := Decisions{ByKind: map[string]int{}, AnsweredBy: map[string]int{}, PerKind: map[string]*Dist{}}
	var reqs []decisionReq
	// (a) task decisions: every entry into needs_decision.
	for _, id := range sortedIDs(ix) {
		t := ix.tasks[id]
		for i, e := range t.Events {
			if !stateEvent(e) || e.To != "needs_decision" {
				continue
			}
			q := decisionReq{kind: "task", ref: "task#" + strconv.FormatInt(id, 10), at: e.At}
			if t.Refs.Disposition != nil {
				q.kind = "disposition"
			}
			for k := i + 1; k < len(t.Events); k++ {
				a := t.Events[k]
				if !stateEvent(a) || a.From != "needs_decision" {
					continue
				}
				at := a.At
				q.answer = &at
				q.answeredBy = "resolver"
				if strings.HasPrefix(a.By, "operator:") {
					q.answeredBy = "operator(web)"
				}
				for c := k + 1; c < len(t.Events); c++ {
					if stateEvent(t.Events[c]) {
						ct := t.Events[c].At
						q.consume = &ct
						break
					}
				}
				if ev := decisionAnswerEvent(ix, "task", id, t.Lane, e.At); ev != nil && ev.DeliveredAt != nil {
					d := *ev.DeliveredAt
					q.deliver = &d
				}
				break
			}
			reqs = append(reqs, q)
		}
	}
	// (b) escalations and (c) lane decisions from the relay stream.
	for i := range ix.snap.Relay {
		e := &ix.snap.Relay[i]
		var kind string
		switch {
		case e.Kind == "job.escalate":
			kind = "escalation"
		case e.Kind == "lane.event" && strings.HasPrefix(e.Text, "[decision-needed]"):
			kind = "lane"
		default:
			continue
		}
		q := decisionReq{kind: kind, ref: kind + "#" + strconv.FormatInt(e.ID, 10), at: e.ReceivedAt}
		if a := decisionAnswerEvent(ix, kind, e.ID, e.OwnerLane, e.ReceivedAt); a != nil {
			at := a.ReceivedAt
			q.answer = &at
			q.answeredBy = "resolver"
			if strings.HasPrefix(a.EventID, "web-") {
				q.answeredBy = "operator(web)"
			}
			if a.DeliveredAt != nil {
				d := *a.DeliveredAt
				q.deliver = &d
			}
			if kind == "escalation" {
				for _, next := range ix.relayJobs[e.JobID] {
					if next.ReceivedAt.After(at) {
						ct := next.ReceivedAt
						q.consume = &ct
						break
					}
				}
			} else {
				q.consumeUnavailable = true
			}
		} else if kind == "escalation" {
			// The job moved on without a recorded answer: the answer time is
			// unknown, so the request is closed but not measured.
			for _, next := range ix.relayJobs[e.JobID] {
				if next.ReceivedAt.After(e.ReceivedAt) && next.Kind != "job.escalate" {
					q.answeredBy = "closed without recorded answer"
					break
				}
			}
			if j := ix.jobs[e.JobID]; j != nil && q.answeredBy == "" {
				for _, je := range j.Events {
					switch je.Kind {
					case "job.completed", "job.joined", "job.lost", "job.revoked", "job.reaped":
						if je.At != nil && je.At.After(e.ReceivedAt) {
							q.answeredBy = "closed without recorded answer"
						}
					}
				}
			}
		}
		reqs = append(reqs, q)
	}
	var toAnswer, toDeliver, toConsume []float64
	perKind := map[string][]float64{}
	answerCov := Coverage{What: "decision requests in window with a recorded answer time", Detail: map[string]int{}}
	deliverCov := Coverage{What: "answered requests with a delivered answer event", Detail: map[string]int{}}
	consumeCov := Coverage{What: "answered requests with a consumption event after the answer", Detail: map[string]int{}}
	for _, q := range reqs {
		if q.answer == nil && q.answeredBy == "" {
			out.Open++
			age := hours(ix.snap.Until.Sub(q.at))
			if out.OldestOpen == nil || age > *out.OldestOpen {
				a := round2(age)
				out.OldestOpen, out.OldestOpenRef = &a, q.ref
			}
		}
		if !inWindow(q.at, ix.snap) {
			continue
		}
		out.Requests++
		out.ByKind[q.kind]++
		answerCov.Total++
		if q.answer == nil {
			if q.answeredBy != "" {
				answerCov.Detail[q.answeredBy]++
			} else {
				answerCov.Detail["still open"]++
			}
			continue
		}
		answerCov.Linked++
		out.AnsweredBy[q.answeredBy]++
		v := hours(q.answer.Sub(q.at))
		toAnswer = append(toAnswer, v)
		perKind[q.kind] = append(perKind[q.kind], v)
		deliverCov.Total++
		if q.deliver != nil {
			deliverCov.Linked++
			toDeliver = append(toDeliver, hours(q.deliver.Sub(*q.answer)))
		} else {
			deliverCov.Detail["no delivered answer event ("+q.kind+")"]++
		}
		consumeCov.Total++
		switch {
		case q.consume != nil:
			consumeCov.Linked++
			toConsume = append(toConsume, hours(q.consume.Sub(*q.answer)))
		case q.consumeUnavailable:
			consumeCov.Detail["no consumption record for lane decisions"]++
		default:
			consumeCov.Detail["no later event ("+q.kind+")"]++
		}
	}
	out.ToAnswer, out.AnswerToDeliver, out.AnswerToConsume = dist(toAnswer), dist(toDeliver), dist(toConsume)
	for k, v := range perKind {
		out.PerKind[k] = dist(v)
	}
	out.Coverage = []Coverage{answerCov, deliverCov, consumeCov}
	return out
}

// ---------------------------------------------------------------- metric 5

// RepairTags are explicit repair markers read from task event notes and
// task comments (see docs/fleet-metrics.md).
var RepairTags = map[string]string{
	"repair:reinject": "reinject",
	"repair:manual":   "manual_recovery",
	"repair:config":   "config_fix",
	"repair:state":    "state_correction",
}

// repairSignals returns the repair classes observed for a task and whether
// at least one linked job had event history to inspect.
func (ix *index) repairSignals(id int64) (map[string]bool, bool) {
	out := map[string]bool{}
	t := ix.tasks[id]
	for _, e := range t.Events {
		for tag, class := range RepairTags {
			if hasTag(e.Note, tag) {
				out[class] = true
			}
		}
	}
	for _, c := range ix.snap.TaskComments[id] {
		for tag, class := range RepairTags {
			if hasTag(c.Body, tag) {
				out[class] = true
			}
		}
	}
	history := false
	for job := range ix.taskJobs[id] {
		j := ix.jobs[job]
		if j == nil || len(j.Events) == 0 {
			continue
		}
		history = true
		finished := false
		for _, e := range j.Events {
			switch e.Kind {
			case "job.reclaim":
				out["reinject"] = true
			case "job.completed", "job.joined", "job.reaped":
				finished = true
			case "job.lost":
				if !finished {
					out["manual_recovery"] = true
				}
			case "job.revoked":
				out["manual_recovery"] = true
			}
			if e.Epoch > 1 {
				out["reinject"] = true
			}
		}
	}
	return out, history
}

func normalPath(ix *index) Normal {
	out := Normal{ByRepair: map[string]int{}}
	cov := Coverage{What: "terminal tasks in window with ≥1 linked job that has event history", Detail: map[string]int{}}
	for _, id := range sortedIDs(ix) {
		t := ix.tasks[id]
		term := terminalAt(t)
		if term == nil || !inWindow(*term, ix.snap) {
			if term == nil && t.State != "backlog" {
				if sig, _ := ix.repairSignals(id); len(sig) > 0 {
					out.OpenRepaired++
					if order := firstTo(t, "claimed"); order != nil {
						age := round2(hours(ix.snap.Until.Sub(order.At)))
						if out.OpenOldest == nil || age > *out.OpenOldest {
							out.OpenOldest = &age
						}
					}
				}
			}
			continue
		}
		out.Terminal++
		cov.Total++
		if t.State == "dropped" {
			out.Dropped++
		}
		sig, history := ix.repairSignals(id)
		if !history && len(sig) == 0 {
			if len(ix.taskJobs[id]) == 0 {
				cov.Detail["no linked job"]++
			} else {
				cov.Detail["linked job(s) without event history on covered machines"]++
			}
			continue
		}
		cov.Linked++
		cov.Detail["best link: "+bestTier(ix.taskJobs[id])]++
		out.Classified++
		if len(sig) > 0 {
			out.Repaired++
			for class := range sig {
				out.ByRepair[class]++
			}
			continue
		}
		if t.State == "merged" {
			out.NormalPath++
		}
	}
	out.Coverage = []Coverage{cov}
	return out
}

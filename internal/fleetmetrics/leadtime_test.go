package fleetmetrics

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	hk10 = "https://github.com/mgh3326/handoffkeep/pull/10"
	hk11 = "https://github.com/mgh3326/handoffkeep/pull/11"
	hk12 = "https://github.com/mgh3326/handoffkeep/pull/12"
	hk13 = "https://github.com/mgh3326/handoffkeep/pull/13"
	hk14 = "https://github.com/mgh3326/handoffkeep/pull/14"
	as5  = "https://github.com/mgh3326/agent-skills/pull/5"
	as6  = "https://github.com/mgh3326/agent-skills/pull/6"
)

func pr(url, merge string, mergedH float64) PRFact {
	u, repo, n := CanonicalPR(url)
	return PRFact{URL: u, Repo: repo, Number: n, MergedAt: atp(mergedH), MergeSHA: merge}
}

// leadFixture: tasks 1 and 6 reach a deploy (5h, 2h), task 2 is merge-only
// (2h), task 3 has no PR, task 4 has no claim event, task 5 awaits deploy,
// task 8 merged before the deploy-record stream began, task 7 is open.
func leadFixture() Snapshot {
	s := snap(0, 48)
	r1 := &store.TaskRefs{PR: hk10}
	s.Tasks = []store.Task{
		task(1, "merged", 0.5, store.TaskRefs{PR: hk10}, mergedChain(1, 2, 3, r1)...),
		task(2, "merged", 0.5, store.TaskRefs{JobID: "2-skills-20260920-0200"}, mergedChain(2, 3, 4, nil)...),
		task(3, "merged", 0.5, store.TaskRefs{}, mergedChain(3, 4, 5, nil)...),
		task(4, "merged", 0.5, store.TaskRefs{PR: as6}, ev(5, "in_progress", "verifying", nil), ev(6, "verifying", "merged", nil)),
		task(5, "merged", 0.5, store.TaskRefs{PR: hk12}, mergedChain(6.5, 6.8, 7, nil)...),
		task(6, "merged", 0.5, store.TaskRefs{PR: hk13}, mergedChain(8, 8.5, 9, nil)...),
		task(7, "in_progress", 39, store.TaskRefs{}, ev(40, "backlog", "claimed", nil), ev(40, "claimed", "in_progress", nil)),
		task(8, "merged", 0, store.TaskRefs{PR: hk14}, mergedChain(0.1, 0.15, 0.2, nil)...),
	}
	s.Relay = []store.RelayEvent{{ID: 1, Kind: "job.joined", JobID: "2-skills-20260920-0200", PR: as5, ReceivedAt: at(3.5)}}
	s.PRs = map[string]PRFact{
		hk10: pr(hk10, "aaaaaaa10", 3),
		as5:  pr(as5, "5555555", 4),
		as6:  pr(as6, "6666666", 6),
		hk12: pr(hk12, "aaaaaaa12", 7),
		hk13: pr(hk13, "aaaaaaa13", 9),
		hk14: pr(hk14, "aaaaaaa14", 0.2),
	}
	s.Deploys = []DeployRecord{
		{Key: "deploy/handoffkeep/a", Service: "handoffkeep", Result: "success", DeployedAt: atp(0.5), DeployedRef: "ccccccc", Format: "json"},
		{Key: "deploy/handoffkeep/b", Service: "handoffkeep", Result: "success", DeployedAt: atp(6), DeployedRef: "ddddddd", IncludedPRs: []string{"handoffkeep#10 (#1 drawer)"}, Format: "json"},
		{Key: "deploy/handoffkeep/c", Service: "handoffkeep", Result: "success", DeployedAt: atp(8), DeployedRef: "eeeeeee", IncludedPRs: []string{"handoffkeep#100 (other)"}, Format: "json"},
		{Key: "deploy/handoffkeep/d", Service: "handoffkeep", Result: "success", DeployedAt: atp(10), DeployedRef: "bbbbbbb", Format: "json"},
		{Key: "deploy/handoffkeep/e", Service: "handoffkeep", Result: "failed", DeployedAt: atp(7.5), DeployedRef: "fffffff", IncludedPRs: []string{"handoffkeep#12"}, Format: "json"},
	}
	s.Contains = map[string]bool{
		"mgh3326/handoffkeep@aaaaaaa13..bbbbbbb": true,
		"mgh3326/handoffkeep@aaaaaaa12..bbbbbbb": false,
		"mgh3326/handoffkeep@aaaaaaa14..ccccccc": true,
	}
	return s
}

func TestLeadTimeGroupsAndPhases(t *testing.T) {
	r := Compute(leadFixture()).LeadTime
	if r.Deploy == nil || r.Deploy.N != 2 || r.Deploy.P50 != 2 || r.Deploy.Max != 5 {
		t.Fatalf("deploy group = %+v, want n=2 p50=2 max=5 (task 6: 8h→10h, task 1: 1h→6h)", r.Deploy)
	}
	if r.MergeOnly == nil || r.MergeOnly.N != 1 || r.MergeOnly.P50 != 2 {
		t.Fatalf("merge-only group = %+v, want n=1 p50=2 (task 2 via relay PR link)", r.MergeOnly)
	}
	if r.Completed != 7 {
		t.Fatalf("completed = %d, want 7", r.Completed)
	}
	if got := r.Phases["install"]; got == nil || got.N != 2 || got.Max != 3 {
		t.Fatalf("install phase = %+v, want n=2 max=3", got)
	}
	if got := r.Phases["implement"]; got == nil || got.N != 3 {
		t.Fatalf("implement phase = %+v, want n=3", got)
	}
	if r.OpenCount != 1 || r.OpenAge == nil || r.OpenAge.P50 != 8 {
		t.Fatalf("open = %d %+v, want 1 task aged 8h", r.OpenCount, r.OpenAge)
	}
	pop := findCoverage(r.Coverage, "tasks merged in window")
	if pop.Linked != 3 || pop.Total != 7 {
		t.Fatalf("population coverage = %+v, want 3/7", pop)
	}
	for key, want := range map[string]int{"no PR link": 1, "no claim event": 1, "awaiting_deploy": 1, "merged before the deploy-record stream began": 1} {
		if pop.Detail[key] != want {
			t.Fatalf("coverage detail %q = %d, want %d (%v)", key, pop.Detail[key], want, pop.Detail)
		}
	}
}

// A task that loses its PR identifier drops out of the value and lowers
// coverage; the remaining value is unchanged and nothing becomes 0h.
func TestLeadTimeMissingPRLowersCoverageNotValue(t *testing.T) {
	s := leadFixture()
	s.Tasks[0].Refs = store.TaskRefs{}
	for i := range s.Tasks[0].Events {
		s.Tasks[0].Events[i].Refs = nil
	}
	r := Compute(s).LeadTime
	if r.Deploy == nil || r.Deploy.N != 1 || r.Deploy.P50 != 2 {
		t.Fatalf("deploy group = %+v, want only task 6 (n=1, 2h)", r.Deploy)
	}
	pop := findCoverage(r.Coverage, "tasks merged in window")
	if pop.Linked != 2 || pop.Detail["no PR link"] != 2 {
		t.Fatalf("coverage = %+v, want 2 linked and 2 without PR link", pop)
	}
	if r.Phases["install"].Max != 1 {
		t.Fatalf("install phase = %+v, want only task 6's 1h", r.Phases["install"])
	}
}

// With no observed value at all, the distribution is absent (nil), never a
// zero-valued summary.
func TestLeadTimeNoObservationIsNil(t *testing.T) {
	s := snap(0, 48)
	s.Tasks = []store.Task{task(3, "merged", 0.5, store.TaskRefs{}, mergedChain(3, 4, 5, nil)...)}
	r := Compute(s).LeadTime
	if r.Deploy != nil || r.MergeOnly != nil {
		t.Fatalf("got %+v / %+v, want nil distributions", r.Deploy, r.MergeOnly)
	}
}

// A deploy-group repo whose records exist but none is dated (unparseable
// bodies, or YAML without deployed_at) is its own unlinked reason: the merge
// is not claimed to predate a stream that exists but did not parse.
func TestLeadTimeUndatedDeployStreamIsNotPreStream(t *testing.T) {
	const bd = "https://github.com/mgh3326/brewdial/pull/3"
	s := snap(0, 48)
	s.Tasks = []store.Task{task(1, "merged", 0.5, store.TaskRefs{PR: bd}, mergedChain(1, 2, 3, nil)...)}
	s.PRs = map[string]PRFact{bd: pr(bd, "bbbbbbb03", 3)}
	s.Deploys = []DeployRecord{
		{Key: "deploy/brewdial-api/x", Service: "brewdial-api", Format: "invalid"},
		{Key: "deploy/brewdial-db/y", Service: "brewdial-db", Format: "yaml"},
	}
	r := Compute(s).LeadTime
	if r.Deploy != nil {
		t.Fatalf("deploy group = %+v, want nil (no usable time is known)", r.Deploy)
	}
	pop := findCoverage(r.Coverage, "tasks merged in window")
	if pop.Linked != 0 || pop.Detail["no dated deploy record for the repo's services"] != 1 || pop.Detail["merged before the deploy-record stream began"] != 0 {
		t.Fatalf("coverage = %+v, want the undated-stream reason, not the pre-stream one", pop)
	}
}

func TestLeadTimeReopenedCycleLinks(t *testing.T) {
	for _, tier := range []string{"refs", "job", "report_path", "note_pr", "note_job", "name", "reused_job", "no_current_link"} {
		t.Run(tier, func(t *testing.T) {
			s := snap(0, 24)
			old := store.TaskRefs{PR: hk10, JobID: "81-old-job", ReportPath: "/jobs/81-old-job/report.md"}
			current := store.TaskRefs{}
			note := ""
			switch tier {
			case "refs":
				current.PR = hk11
			case "job", "reused_job":
				current.JobID = "current-job"
			case "report_path":
				current.ReportPath = "/jobs/current-job/report.md"
			case "note_pr":
				note = hk11
			case "note_job":
				note = "current-job"
			}
			events := mergedChain(0.1, 0.5, 1, &old)
			// The reopen reason may describe the previous PR/job. It is not
			// linkage for the new completion.
			events = append(events, ev(4, "merged", "reopened", nil, hk10+" 81-old-job"))
			events = append(events, ev(6, "reopened", "claimed", &current, note), ev(7, "claimed", "verifying", &current), ev(8, "verifying", "merged", &current))
			s.Tasks = []store.Task{task(81, "merged", 0, current, events...)}
			s.Relay = []store.RelayEvent{{JobID: "81-old-job", PR: hk10, ReceivedAt: at(1)}}
			s.Jobs = []Job{{JobID: "81-old-job", Events: []JobEvent{{Kind: "start", At: atp(0.1)}, {Kind: "end", At: atp(7), PR: hk10}}}}
			if tier != "refs" && tier != "note_pr" && tier != "no_current_link" {
				job := "current-job"
				if tier == "name" {
					job = "81-new-job"
				}
				s.Relay = append(s.Relay, store.RelayEvent{JobID: job, PR: hk11, ReceivedAt: at(8)})
				s.Jobs = append(s.Jobs, Job{JobID: job, Events: []JobEvent{{Kind: "start", At: atp(6)}, {Kind: "end", At: atp(8), PR: hk11}}})
				if tier == "reused_job" {
					// A current explicit job link must not inherit that job's
					// PR evidence from before the cycle, in either source.
					s.Relay = append(s.Relay, store.RelayEvent{JobID: job, PR: hk10, EventTime: atp(1), ReceivedAt: at(7)})
					s.Jobs[1].Events = append(s.Jobs[1].Events, JobEvent{At: atp(1), PR: hk10}, JobEvent{PR: hk10})
				}
			}
			s.PRs = map[string]PRFact{hk10: pr(hk10, "aaaaaaa10", 1), hk11: pr(hk11, "aaaaaaa11", 8)}
			s.Deploys = []DeployRecord{
				{Service: "handoffkeep", Result: "success", DeployedAt: atp(2), Format: "json"},
				{Service: "handoffkeep", Result: "success", DeployedAt: atp(9), IncludedPRs: []string{"handoffkeep#11"}, Format: "json"},
			}
			ix := buildIndex(&s)
			if _, ok := ix.taskPRs[81][hk10]; !ok {
				t.Fatal("historical linkage was removed from the full index")
			}
			got := leadTime(ix)
			coverage := findCoverage(got.Coverage, "tasks merged in window")
			if tier == "no_current_link" {
				if got.Deploy != nil || coverage.Detail["no PR link"] != 1 {
					t.Fatalf("old PR inherited without current evidence: %+v coverage=%+v", got.Deploy, coverage)
				}
			} else if got.Deploy == nil || got.Deploy.N != 1 || got.Deploy.P50 != 3 || coverage.Linked != 1 {
				t.Fatalf("current cycle lead time=%+v coverage=%+v, want 6h→9h", got.Deploy, coverage)
			}
		})
	}
}

// A task reopened twice: the lead time and PR link come from the LATEST
// cycle only. A cycle start taken from the first reopen would leak PR #12
// from the middle cycle and double the lead time (v86-pi, mutant 1).
func TestLeadTimeTwiceReopenedUsesLatestCycle(t *testing.T) {
	s := snap(0, 24)
	first := store.TaskRefs{PR: hk10, JobID: "82-first"}
	middle := store.TaskRefs{PR: hk12, JobID: "82-middle"}
	current := store.TaskRefs{PR: hk11}
	events := mergedChain(0.1, 0.5, 1, &first)
	events = append(events, ev(2, "merged", "reopened", nil, "first reopen"))
	events = append(events, ev(3, "reopened", "claimed", &middle), ev(3.5, "claimed", "verifying", &middle), ev(4, "verifying", "merged", &middle))
	events = append(events, ev(5, "merged", "reopened", nil, "second reopen"))
	events = append(events, ev(6, "reopened", "claimed", &current), ev(7, "claimed", "verifying", &current), ev(8, "verifying", "merged", &current))
	s.Tasks = []store.Task{task(82, "merged", 0, current, events...)}
	s.PRs = map[string]PRFact{hk10: pr(hk10, "aaaaaaa10", 1), hk12: pr(hk12, "aaaaaaa12", 4), hk11: pr(hk11, "aaaaaaa11", 8)}
	s.Deploys = []DeployRecord{
		{Service: "handoffkeep", Result: "success", DeployedAt: atp(4.5), IncludedPRs: []string{"handoffkeep#12"}, Format: "json"},
		{Service: "handoffkeep", Result: "success", DeployedAt: atp(9), IncludedPRs: []string{"handoffkeep#11"}, Format: "json"},
	}
	got := leadTime(buildIndex(&s))
	if got.Deploy == nil || got.Deploy.N != 1 || got.Deploy.P50 != 3 {
		t.Fatalf("twice-reopened lead time=%+v, want the latest cycle 6h→9h (3h)", got.Deploy)
	}
}

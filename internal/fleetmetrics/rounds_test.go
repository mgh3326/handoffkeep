package fleetmetrics

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func intp(n int) *int { return &n }

// roundsFixture: lineage {1, 2 (origin_task 1)} has 3 verify rounds with one
// regression-tagged fix; task 3 has 5 rounds; task 4 merged with no verify
// transition (rounds unknown); task 5 is terminal before the window.
func roundsFixture() Snapshot {
	s := snap(10, 58)
	s.Tasks = []store.Task{
		task(1, "merged", 0, store.TaskRefs{},
			ev(11, "backlog", "claimed", nil), ev(11, "claimed", "in_progress", nil),
			ev(12, "in_progress", "verifying", nil),
			ev(13, "verifying", "in_progress", nil, "tester BLOCKER F1 cause:regression"),
			ev(14, "in_progress", "verifying", nil), ev(15, "verifying", "merged", nil)),
		task(2, "merged", 16, store.TaskRefs{OriginTask: 1},
			ev(17, "backlog", "claimed", nil), ev(17, "claimed", "in_progress", nil),
			ev(18, "in_progress", "verifying", nil), ev(19, "verifying", "merged", nil)),
		task(3, "merged", 0, store.TaskRefs{},
			ev(11, "backlog", "claimed", nil), ev(11, "claimed", "in_progress", nil),
			ev(12, "in_progress", "verifying", nil), ev(13, "verifying", "in_progress", nil),
			ev(14, "in_progress", "verifying", nil), ev(15, "verifying", "in_progress", nil),
			ev(16, "in_progress", "verifying", nil), ev(17, "verifying", "in_progress", nil, "cause:requirement changed AC2"),
			ev(18, "in_progress", "verifying", nil), ev(19, "verifying", "in_progress", nil),
			ev(20, "in_progress", "verifying", nil), ev(21, "verifying", "merged", nil)),
		task(4, "merged", 0, store.TaskRefs{}, ev(11, "backlog", "claimed", nil), ev(11, "claimed", "in_progress", nil), ev(12, "in_progress", "merged", nil)),
		task(5, "merged", 0, store.TaskRefs{}, mergedChain(1, 2, 3, nil)...),
	}
	s.Reps = []Rep{
		{ID: 1, Task: "3", Role: "impl", Rounds: intp(5), RecordedAt: at(22)},
		{ID: 2, Task: "3", Role: "verify", Rounds: intp(1), RecordedAt: at(22)},
		{ID: 3, Task: "1", Role: "impl", Rounds: intp(1), RecordedAt: at(16)},
	}
	s.RepSources = []string{"fixture"}
	return s
}

func TestRoundsPerLineage(t *testing.T) {
	r := Compute(roundsFixture()).Rounds
	if r.PerLineage == nil || r.PerLineage.N != 2 || r.PerLineage.P50 != 3 || r.PerLineage.Max != 5 {
		t.Fatalf("per lineage = %+v, want n=2 p50=3 max=5", r.PerLineage)
	}
	if r.Lineages != 2 || r.OverThree != 1 {
		t.Fatalf("lineages=%d over3=%d, want 2 and 1", r.Lineages, r.OverThree)
	}
	if r.FixRounds != 5 || r.Cause["regression"] != 1 || r.Cause["requirement"] != 1 {
		t.Fatalf("fix=%d cause=%v, want 5 fix rounds, 1 regression, 1 requirement", r.FixRounds, r.Cause)
	}
	if r.RepsAgree != 1 || r.RepsDisagree != 1 {
		t.Fatalf("reps agree/disagree = %d/%d, want 1/1 (task 3: 5=5; lineage 1: 1≠3)", r.RepsAgree, r.RepsDisagree)
	}
	cov := findCoverage(r.Coverage, "lineages (task")
	if cov.Linked != 2 || cov.Total != 3 || cov.Detail["no verify transition recorded"] != 1 {
		t.Fatalf("coverage = %+v, want 2/3 with one lineage lacking a verify transition", cov)
	}
	cause := findCoverage(r.Coverage, "fix rounds")
	if cause.Linked != 2 || cause.Total != 5 {
		t.Fatalf("cause coverage = %+v, want 2/5", cause)
	}
}

// Removing task 3's transitions (the identifier the metric needs) lowers
// coverage; it must not enter the distribution as 0 rounds.
func TestRoundsMissingTransitionsLowerCoverageNotValue(t *testing.T) {
	s := roundsFixture()
	s.Tasks[2].Events = []store.TaskEvent{ev(11, "backlog", "claimed", nil), ev(21, "claimed", "merged", nil)}
	r := Compute(s).Rounds
	if r.PerLineage == nil || r.PerLineage.N != 1 || r.PerLineage.P50 != 3 {
		t.Fatalf("per lineage = %+v, want only lineage {1,2} with 3 rounds", r.PerLineage)
	}
	cov := findCoverage(r.Coverage, "lineages (task")
	if cov.Linked != 1 || cov.Detail["no verify transition recorded"] != 2 {
		t.Fatalf("coverage = %+v, want 1 linked, 2 without verify transitions", cov)
	}
}

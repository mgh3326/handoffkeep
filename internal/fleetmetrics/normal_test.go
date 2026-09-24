package fleetmetrics

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func job(id string, kinds ...string) Job {
	j := Job{JobID: id, Machine: "m1", Role: "builder", OwnerLane: "b-" + id}
	for i, k := range kinds {
		e := jev(k, float64(i+1))
		e.Seq = i + 1
		j.Events = append(j.Events, e)
	}
	return j
}

// normalFixture: 30 normal (its job.lost comes after reaped, as the sentinel
// writes it in practice), 31 re-injected, 32 dropped, 33 no linked job,
// 34 lost before finishing, 35 tagged config fix, 36 linked job without
// event history, 37 open with a re-inject.
func normalFixture() Snapshot {
	s := snap(0, 48)
	s.Tasks = []store.Task{
		task(30, "merged", 0, store.TaskRefs{JobID: "30-a-job"}, mergedChain(1, 5, 7, nil)...),
		task(31, "merged", 0, store.TaskRefs{JobID: "31-b-job"}, mergedChain(1, 5, 7, nil)...),
		task(32, "dropped", 0, store.TaskRefs{JobID: "32-c-job"}, ev(1, "backlog", "claimed", nil), ev(2, "claimed", "dropped", nil)),
		task(33, "merged", 0, store.TaskRefs{}, mergedChain(1, 5, 7, nil)...),
		task(34, "merged", 0, store.TaskRefs{JobID: "34-e-job"}, mergedChain(1, 5, 7, nil)...),
		task(35, "merged", 0, store.TaskRefs{JobID: "35-f-job"}, mergedChain(1, 5, 7, nil)...),
		task(36, "merged", 0, store.TaskRefs{JobID: "remote-only-job"}, mergedChain(1, 5, 7, nil)...),
		task(37, "in_progress", 0, store.TaskRefs{JobID: "37-g-job"}, ev(2, "backlog", "claimed", nil), ev(2, "claimed", "in_progress", nil)),
	}
	s.Jobs = []Job{
		job("30-a-job", "job.claim", "job.spawned", "job.completed", "job.reaped", "job.lost"),
		job("31-b-job", "job.claim", "job.spawned", "job.reclaim", "job.completed"),
		job("32-c-job", "job.claim", "job.spawned", "job.completed"),
		job("34-e-job", "job.claim", "job.spawned", "job.lost"),
		job("35-f-job", "job.claim", "job.spawned", "job.completed"),
		job("37-g-job", "job.claim", "job.spawned", "job.reclaim"),
	}
	s.TaskComments = map[int64][]Comment{35: {{ID: 1, Body: "repair:config — hosts.toml capacity fixed by operator-desk", At: at(6)}}}
	return s
}

func TestNormalPathShare(t *testing.T) {
	r := Compute(normalFixture()).NormalPath
	if r.Terminal != 7 || r.Classified != 5 || r.NormalPath != 1 || r.Repaired != 3 || r.Dropped != 1 {
		t.Fatalf("terminal %d classified %d normal %d repaired %d dropped %d, want 7/5/1/3/1", r.Terminal, r.Classified, r.NormalPath, r.Repaired, r.Dropped)
	}
	for class, want := range map[string]int{"reinject": 1, "manual_recovery": 1, "config_fix": 1} {
		if r.ByRepair[class] != want {
			t.Fatalf("by repair %s = %d, want %d (%v)", class, r.ByRepair[class], want, r.ByRepair)
		}
	}
	if r.OpenRepaired != 1 {
		t.Fatalf("open repaired = %d, want 1", r.OpenRepaired)
	}
	cov := findCoverage(r.Coverage, "terminal tasks")
	if cov.Linked != 5 || cov.Total != 7 || cov.Detail["no linked job"] != 1 || cov.Detail["linked job(s) without event history on covered machines"] != 1 {
		t.Fatalf("coverage = %+v", cov)
	}
}

// Dropping task 30's job link (the identifier) makes it unclassified: the
// normal count falls with coverage, and the task is not assumed normal.
func TestNormalPathMissingJobLowersCoverageNotValue(t *testing.T) {
	s := normalFixture()
	s.Tasks[0].Refs = store.TaskRefs{}
	s.Jobs[0].JobID = "unlinked-a-job"
	r := Compute(s).NormalPath
	if r.Classified != 4 || r.NormalPath != 0 {
		t.Fatalf("classified %d normal %d, want 4 and 0", r.Classified, r.NormalPath)
	}
	cov := findCoverage(r.Coverage, "terminal tasks")
	if cov.Linked != 4 || cov.Detail["no linked job"] != 2 {
		t.Fatalf("coverage = %+v", cov)
	}
}

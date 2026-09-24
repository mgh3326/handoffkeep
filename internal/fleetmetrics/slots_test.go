package fleetmetrics

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func mins(m float64) float64 { return m / 60 }

func run(fromMin, toMin float64, status string) StatusRun {
	return StatusRun{From: at(mins(fromMin)), To: at(mins(toMin)), Status: status}
}

// slotsFixture: one machine with 2 task slots over a 60-minute window and a
// backlog task ready the whole time.
//   - J1 live builder: sentinel working 0–29, idle 30–60 (waiting on its tester)
//   - J2 builder: claim 10, reaped 20, no sentinel samples (event-inferred)
//   - J4 builder: claim 0, no terminal record, its task merged at 5 → slot ends at 5
//
// Empty-with-ready slot-minutes: 0–4 → 0, 5–9 → 5, 10–19 → 0, 20–29 → 10,
// 30–59 → 30 (J1 is held, not empty) = 45.
func slotsFixture() Snapshot {
	s := snap(0, 1)
	s.Tasks = []store.Task{
		task(9, "merged", -1, store.TaskRefs{JobID: "j4-builder"},
			ev(-0.9, "backlog", "claimed", nil), ev(-0.9, "claimed", "in_progress", nil), ev(mins(5), "in_progress", "merged", nil)),
		task(10, "backlog", -1, store.TaskRefs{}),
	}
	s.Jobs = []Job{
		{JobID: "j1-builder", Machine: "m1", Role: "builder", OwnerLane: "b1", Live: true,
			Events: []JobEvent{jev("job.claim", 0), jev("job.spawned", 0)},
			Status: []StatusRun{run(0, 29.9, "working"), run(30, 60, "idle")}},
		{JobID: "j2-builder", Machine: "m1", Role: "builder", OwnerLane: "b2",
			Events: []JobEvent{jev("job.claim", mins(10)), jev("job.spawned", mins(10)), jev("job.reaped", mins(20))}},
		{JobID: "j4-builder", Machine: "m1", Role: "builder", OwnerLane: "b4",
			Events: []JobEvent{jev("job.claim", 0), jev("job.spawned", 0)}},
	}
	s.JobRoots = []JobRoot{{Path: "/fixture", Machine: "m1"}}
	s.Slots = []SlotCapacity{{Machine: "m1", Slots: 2, Source: "fixture"}}
	return s
}

func TestSlotsHeldIdleIsNotEmpty(t *testing.T) {
	r := Compute(slotsFixture()).EmptySlots
	if r.SlotMinutes != 120 || r.Working != 45 || r.HeldIdle != 30 || r.Empty != 45 || r.Unobserved != 0 {
		t.Fatalf("partition = slot %d working %d held %d empty %d unobserved %d, want 120/45/30/45/0", r.SlotMinutes, r.Working, r.HeldIdle, r.Empty, r.Unobserved)
	}
	if r.EmptyWithReady != 45 || r.EmptyWithReadyMax != 45 {
		t.Fatalf("empty with ready = %d…%d, want 45…45", r.EmptyWithReady, r.EmptyWithReadyMax)
	}
	if r.ByReason["ready work, eligibility unrecorded"] != 45 || len(r.ByReason) != 1 {
		t.Fatalf("by reason = %v, want only 45 ready-work minutes", r.ByReason)
	}
	units := findCoverage(r.Coverage, "builder units")
	if units.Linked != 3 || units.Total != 3 {
		t.Fatalf("unit coverage = %+v, want 3/3 (J4 ends with its task)", units)
	}
}

// A worker (tester) WORKING for the builder's lane makes the task unit
// working even while the builder itself is idle.
func TestSlotsWorkerWorkingCountsForUnit(t *testing.T) {
	s := slotsFixture()
	s.Jobs = append(s.Jobs, Job{JobID: "t1-verify", Machine: "m1", Role: "worker", OwnerLane: "b1",
		Events: []JobEvent{jev("job.claim", mins(30)), jev("job.reaped", mins(40))},
		Status: []StatusRun{run(30, 39.9, "working")}})
	r := Compute(s).EmptySlots
	if r.Working != 55 || r.HeldIdle != 20 || r.EmptyWithReady != 45 {
		t.Fatalf("working %d held %d empty-with-ready %d, want 55/20/45", r.Working, r.HeldIdle, r.EmptyWithReady)
	}
}

// J3 has no terminal record, is not live and links to no task: from its last
// event on it may or may not hold a slot. Those minutes widen the range and
// lower coverage; they are not counted as empty.
func TestSlotsUnobservedBuilderLowersCoverageNotValue(t *testing.T) {
	s := slotsFixture()
	s.Jobs = append(s.Jobs, Job{JobID: "j3-builder", Machine: "m1", Role: "builder", OwnerLane: "b3",
		Events: []JobEvent{jev("job.claim", mins(40)), jev("job.spawned", mins(40))}})
	r := Compute(s).EmptySlots
	if r.EmptyWithReady != 25 || r.EmptyWithReadyMax != 45 {
		t.Fatalf("empty with ready = %d…%d, want 25…45", r.EmptyWithReady, r.EmptyWithReadyMax)
	}
	if r.Unobserved != 20 {
		t.Fatalf("unobserved = %d, want 20", r.Unobserved)
	}
	minutes := findCoverage(r.Coverage, "machine-minutes where")
	if minutes.Linked != 40 || minutes.Total != 60 {
		t.Fatalf("minute coverage = %+v, want 40/60", minutes)
	}
	units := findCoverage(r.Coverage, "builder units")
	if units.Linked != 3 || units.Total != 4 {
		t.Fatalf("unit coverage = %+v, want 3/4", units)
	}
}

// Capacity declared only from minute 30: the first half is out of the
// denominator and reported as capacity coverage, never as empty slots.
func TestSlotsCapacityTimeline(t *testing.T) {
	s := slotsFixture()
	from := at(mins(30))
	s.Slots = []SlotCapacity{{Machine: "m1", Slots: 2, From: &from, Source: "fixture"}}
	r := Compute(s).EmptySlots
	if r.SlotMinutes != 60 || r.EmptyWithReady != 30 {
		t.Fatalf("slot-minutes %d empty-with-ready %d, want 60 and 30", r.SlotMinutes, r.EmptyWithReady)
	}
	c := findCoverage(r.Coverage, "machine-minutes with a defined")
	if c.Linked != 30 || c.Total != 60 {
		t.Fatalf("capacity coverage = %+v, want 30/60", c)
	}
}

func TestSlotsMachineWithoutHistoryIsCoverageOnly(t *testing.T) {
	s := slotsFixture()
	s.Slots = append(s.Slots, SlotCapacity{Machine: "m2", Slots: 5})
	r := Compute(s).EmptySlots
	if r.SlotMinutes != 120 {
		t.Fatalf("slot-minutes = %d, want 120 (m2 has no job history and is not counted)", r.SlotMinutes)
	}
	c := findCoverage(r.Coverage, "machines with task slots")
	if c.Linked != 1 || c.Total != 2 {
		t.Fatalf("machine coverage = %+v, want 1/2", c)
	}
}

// A builder with no terminal record whose linked task ended before the
// window and was not refetched (Events == nil): the task's UpdatedAt ends
// the unit, so it holds no slot in the window and costs no coverage.
func TestSlotsUnrefetchedTerminalTaskEndsUnit(t *testing.T) {
	s := slotsFixture()
	old := task(11, "merged", -30, store.TaskRefs{JobID: "j5-builder"})
	old.Events, old.UpdatedAt = nil, at(-20)
	s.Tasks = append(s.Tasks, old)
	s.Jobs = append(s.Jobs, Job{JobID: "j5-builder", Machine: "m1", Role: "builder", OwnerLane: "b5",
		Events: []JobEvent{jev("job.claim", -30), jev("job.spawned", -30)}})
	r := Compute(s).EmptySlots
	if r.EmptyWithReady != 45 || r.EmptyWithReadyMax != 45 || r.Unobserved != 0 {
		t.Fatalf("empty with ready %d…%d unobserved %d, want 45…45 and 0", r.EmptyWithReady, r.EmptyWithReadyMax, r.Unobserved)
	}
	if c := findCoverage(r.Coverage, "machine-minutes where"); c.Linked != 60 {
		t.Fatalf("minute coverage = %+v, want 60/60", c)
	}
}

// The only other task was not refetched (Events == nil) and last changed at
// minute 30: before that its state is unknown, so whether ready work existed
// is unknown too. Those empty minutes are "ready work unknown", never
// "backlog empty". Empty per minute: 5–9 → 5, 20–29 → 10, 30–59 → 30.
func TestSlotsUnknownReadyIsNotBacklogEmpty(t *testing.T) {
	s := slotsFixture()
	s.Tasks[1] = task(10, "in_progress", -1, store.TaskRefs{})
	s.Tasks[1].Events, s.Tasks[1].UpdatedAt = nil, at(mins(30))
	r := Compute(s).EmptySlots
	if r.Empty != 45 || r.EmptyWithReady != 0 {
		t.Fatalf("empty %d empty-with-ready %d, want 45 and 0", r.Empty, r.EmptyWithReady)
	}
	if r.ByReason["ready work unknown"] != 15 || r.ByReason["no ready work (backlog empty)"] != 30 {
		t.Fatalf("by reason = %v, want 15 unknown and 30 backlog-empty", r.ByReason)
	}
}

// Two live builders on a 1-slot machine, one WORKING and one idle for the
// whole hour: working takes the slot, the idle one does not fit, and the
// excess is overflow — held-idle never exceeds the capacity left over.
func TestSlotsOverCapacityIsOverflowNotHeld(t *testing.T) {
	s := snap(0, 1)
	s.Tasks = []store.Task{task(10, "backlog", -1, store.TaskRefs{})}
	s.Jobs = []Job{
		{JobID: "ja-builder", Machine: "m1", Role: "builder", OwnerLane: "ba", Live: true,
			Events: []JobEvent{jev("job.claim", 0)}, Status: []StatusRun{run(0, 60, "working")}},
		{JobID: "jb-builder", Machine: "m1", Role: "builder", OwnerLane: "bb", Live: true,
			Events: []JobEvent{jev("job.claim", 0)}, Status: []StatusRun{run(0, 60, "idle")}},
	}
	s.JobRoots = []JobRoot{{Path: "/fixture", Machine: "m1"}}
	s.Slots = []SlotCapacity{{Machine: "m1", Slots: 1, Source: "fixture"}}
	r := Compute(s).EmptySlots
	if r.SlotMinutes != 60 || r.Working != 60 || r.HeldIdle != 0 || r.Empty != 0 || r.Overflow != 60 {
		t.Fatalf("slot %d working %d held %d empty %d overflow %d, want 60/60/0/0/60", r.SlotMinutes, r.Working, r.HeldIdle, r.Empty, r.Overflow)
	}
}

// A builder linked to two tasks holds its slot until the later one ends
// (minute 40), not the first (minute 10).
func TestSlotsUnitEndsWithLastLinkedTask(t *testing.T) {
	s := snap(0, 1)
	refs := store.TaskRefs{JobID: "j6-builder"}
	s.Tasks = []store.Task{
		task(10, "backlog", -1, store.TaskRefs{}),
		task(12, "merged", -1, refs, mergedChain(-0.9, -0.8, mins(10), nil)...),
		task(13, "merged", -1, refs, mergedChain(-0.9, -0.8, mins(40), nil)...),
	}
	s.Jobs = []Job{{JobID: "j6-builder", Machine: "m1", Role: "builder", OwnerLane: "b6",
		Events: []JobEvent{jev("job.claim", 0), jev("job.spawned", 0)}}}
	s.JobRoots = []JobRoot{{Path: "/fixture", Machine: "m1"}}
	s.Slots = []SlotCapacity{{Machine: "m1", Slots: 1, Source: "fixture"}}
	r := Compute(s).EmptySlots
	if r.Working+r.HeldIdle != 40 || r.Empty != 20 || r.EmptyWithReady != 20 {
		t.Fatalf("occupied %d empty %d empty-with-ready %d, want 40/20/20", r.Working+r.HeldIdle, r.Empty, r.EmptyWithReady)
	}
	if units := findCoverage(r.Coverage, "builder units"); units.Linked != 1 || units.Total != 1 {
		t.Fatalf("unit coverage = %+v, want 1/1", units)
	}
}

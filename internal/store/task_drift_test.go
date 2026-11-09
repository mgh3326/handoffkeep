package store

import (
	"fmt"
	"testing"
	"time"
)

// driftAt reads the drift report at now, failing the test on error.
func driftAt(t *testing.T, s *Store, now time.Time, mergedDays int) TaskDrift {
	t.Helper()
	rep, err := s.TaskDrift(t.Context(), now, mergedDays)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func driftIDs(c TaskDriftClass) map[int64]bool {
	out := map[int64]bool{}
	for _, x := range c.Tasks {
		out[x.ID] = true
	}
	return out
}

func TestCheckDriftMergedDaysBounds(t *testing.T) {
	for _, days := range []int{1, DriftDefaultMergedDays, MaxDriftMergedDays} {
		if err := CheckDriftMergedDays(days); err != nil {
			t.Fatalf("days=%d refused: %v", days, err)
		}
	}
	for _, days := range []int{0, -1, MaxDriftMergedDays + 1} {
		if err := CheckDriftMergedDays(days); err == nil {
			t.Fatalf("days=%d accepted", days)
		}
	}
}

// TestDriftSettledWithOpenDecision is class (a): a settled task whose decision
// request is open. An unsettled task with an open request is not drift.
func TestDriftSettledWithOpenDecision(t *testing.T) {
	s, pool := searchTestStore(t)
	settled := flowTask(t, s, pool, "implement", StateMerged, false)
	open := flowTask(t, s, pool, "implement", StateReviewing, false)
	for _, id := range []int64{settled.ID, open.ID} {
		if _, err := pool.Exec(t.Context(), `UPDATE tasks SET refs=refs||jsonb_build_object('decision_request',jsonb_build_object('id',$2::text,'status','open')) WHERE id=$1`, id, fmt.Sprintf("dr-%d-1", id)); err != nil {
			t.Fatal(err)
		}
	}
	rep := driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays)
	ids := driftIDs(rep.SettledOpenDecision)
	if !ids[settled.ID] || ids[open.ID] {
		t.Fatalf("settled_open_decision %+v", rep.SettledOpenDecision)
	}
	for _, x := range rep.SettledOpenDecision.Tasks {
		if x.ID == settled.ID && x.DecisionRequest != fmt.Sprintf("dr-%d-1", settled.ID) {
			t.Fatalf("decision request id %q", x.DecisionRequest)
		}
	}
}

// TestDriftMergedPRNotMerged is class (b): an unsettled task whose PR a merged
// task already names. A task on a PR that is not merged is not drift.
func TestDriftMergedPRNotMerged(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(301)
	done := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, done.ID, pr)
	lagging := flowTask(t, s, pool, "implement", StateReviewing, false)
	setRefsPR(t, s, lagging.ID, pr)
	open := flowTask(t, s, pool, "implement", StateReviewing, false)
	setRefsPR(t, s, open.ID, testPR(302))
	ids := driftIDs(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).MergedPRNotMerged)
	if !ids[lagging.ID] || ids[done.ID] || ids[open.ID] {
		t.Fatalf("merged_pr_not_merged ids %v", ids)
	}
}

// TestDriftMergedWithoutDeploy is class (c): a merged task older than
// merged_days that no deploy record lists. The age is checked against now, so
// the same task is listed only once the cutoff passes it.
func TestDriftMergedWithoutDeploy(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(311)
	stale := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, stale.ID, pr)
	noPR := flowTask(t, s, pool, "implement", StateMerged, false)
	deployed := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, deployed.ID, testPR(312))
	putDeploy(t, s, "deploy/svc/20261105T050000Z", "svc", "success", []string{testPR(312)})

	now := time.Now().UTC()
	if ids := driftIDs(driftAt(t, s, now, DriftDefaultMergedDays).MergedWithoutDeploy); len(ids) != 0 {
		t.Fatalf("fresh merges listed: %v", ids)
	}
	ids := driftIDs(driftAt(t, s, now.Add(72*time.Hour), DriftDefaultMergedDays).MergedWithoutDeploy)
	if !ids[stale.ID] || !ids[noPR.ID] || ids[deployed.ID] {
		t.Fatalf("merged_without_deploy ids %v", ids)
	}
}

// TestDriftIdleStarted is class (d): a researching or developing task with no
// activity for started_stalled_hours. A reviewing task is not in the class.
func TestDriftIdleStarted(t *testing.T) {
	s, pool := searchTestStore(t)
	idle := flowTask(t, s, pool, "implement", StateDeveloping, false)
	review := flowTask(t, s, pool, "implement", StateReviewing, false)
	now := time.Now().UTC()
	if ids := driftIDs(driftAt(t, s, now, DriftDefaultMergedDays).IdleStarted); ids[idle.ID] {
		t.Fatal("a fresh task is listed as idle")
	}
	later := driftAt(t, s, now.Add(72*time.Hour), DriftDefaultMergedDays)
	ids := driftIDs(later.IdleStarted)
	if !ids[idle.ID] || ids[review.ID] {
		t.Fatalf("idle_started ids %v", ids)
	}
	if later.StartedStalledHours != DefaultStartedStalledHours {
		t.Fatalf("started_stalled_hours %d", later.StartedStalledHours)
	}
}

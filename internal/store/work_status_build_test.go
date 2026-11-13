package store

import (
	"testing"
	"time"
)

// BuildWorkStatus is the classification both work status sources share
// (hk's tables and YouTrack's issues, hk 4430). It needs no database.
func TestBuildWorkStatusClassifiesRows(t *testing.T) {
	now := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	hk, other := "handoffkeep", "panewire"
	deployed := now.Add(-2 * time.Hour)
	since := now.Add(-30 * time.Minute)
	early, late := now.Add(-3*time.Hour), now.Add(-time.Hour)
	parent := int64(9)
	rows := []WorkStatusRow{
		{ID: 1, Title: "low", Project: &hk, State: StateBacklog, Priority: 0, CreatedAt: early},
		{ID: 2, Title: "high", Project: &hk, State: StateTriage, Priority: 5, CreatedAt: late, ParentID: &parent, BundleID: &parent, BundleTitle: "[bundle b B01]"},
		{ID: 3, Title: "dev", Project: &hk, State: StateDeveloping, UpdatedAt: early, InStateSince: &since, JobID: "j"},
		{ID: 4, Title: "review", Project: &hk, State: StateReviewing, UpdatedAt: early},
		{ID: 5, Title: "waits deploy", Project: &hk, State: StateMerged, MergedAt: &late},
		{ID: 6, Title: "live", Project: &hk, State: StateMerged, MergedAt: &early},
		{ID: 7, Title: "no deploy record", Project: &other, State: StateMerged},
		{ID: 8, Title: "[bundle b B01]", Project: &hk, State: StateBacklog},
	}
	got := BuildWorkStatus(now, rows, WorkStatusInputs{LastDeploy: map[string]time.Time{hk: deployed}, Projects: []string{"empty", hk}})
	want := WorkStatusSummary{TotalOpen: 5, Backlog: 2, InWork: 1, InReview: 1, WaitingDeploy: 1, MergedNoDeployProject: 1, MergedBeforeDeploy: 1, BundleParents: 1}
	if got.Summary != want {
		t.Fatalf("summary=%+v\nwant %+v", got.Summary, want)
	}
	if len(got.Backlog) != 2 || got.Backlog[0].ID != 2 || got.Backlog[0].BundleTitle != "[bundle b B01]" || got.Backlog[1].ID != 1 {
		t.Fatalf("backlog=%+v", got.Backlog)
	}
	if len(got.InWork) != 2 || got.InWork[0].ID != 3 || got.InWork[0].AgeSeconds != 1800 || got.InWork[1].ID != 4 {
		t.Fatalf("in work=%+v", got.InWork)
	}
	if len(got.Projects) != 3 || got.Projects[0].Project != "empty" || got.Projects[1].Project != hk || got.Projects[1].Counts.TotalOpen != 5 {
		t.Fatalf("projects=%+v", got.Projects)
	}
	if got.Queued == nil {
		t.Fatal("queued must be an empty list, not null")
	}
}

func TestWorkStatusMovementDaysCoverEightKSTDays(t *testing.T) {
	now := time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC) // 2026-11-14 01:00 KST
	days := WorkStatusMovementDays(now, map[string]int{"2026-11-14": 2}, map[string]int{"2026-11-07": 1})
	if len(days) != 8 || days[0].Date != "2026-11-07" || days[0].Closed != 1 || days[7].Date != "2026-11-14" || days[7].Created != 2 {
		t.Fatalf("days=%+v", days)
	}
}

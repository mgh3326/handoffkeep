package store

import (
	"fmt"
	"testing"
	"time"
)

func TestWorkStatusRead(t *testing.T) {
	st := schemaStore(t, "work_status")
	ctx := t.Context()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	// Configure projects in task_projects
	for _, proj := range []string{"handoffkeep", "panewire", "ParkingArcade"} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO task_projects(name, created_by, created_at) VALUES($1, 'test', $2) ON CONFLICT (name) DO NOTHING`, proj, now); err != nil {
			t.Fatal(err)
		}
	}

	// Configure deploy services:
	// handoffkeep has deploy service with success record at 2026-11-12T07:00:00Z
	// panewire-hub maps to panewire with success record at 2026-11-11T12:00:00Z
	// ParkingArcade has NO deploy service / record.
	if _, err := st.PutDeployService(ctx, DeployService{
		Name:           "handoffkeep",
		Repo:           "mgh3326/handoffkeep",
		RecordPrefixes: []string{"deploy/handoffkeep/"},
		Active:         true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutDeployService(ctx, DeployService{
		Name:           "panewire-hub",
		Repo:           "mgh3326/panewire",
		RecordPrefixes: []string{"deploy/panewire-hub/", "deploy/panewire/"},
		Active:         true,
	}); err != nil {
		t.Fatal(err)
	}

	hkDeployTime := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	pwDeployTime := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	// Seed deploy records into documents
	hkDeployDoc := fmt.Sprintf(`{"schema":"deploy-record/v0","service":"handoffkeep","result":"success","deployed_at":"%s"}`, hkDeployTime.Format(time.RFC3339))
	if _, err := st.pool.Exec(ctx, `INSERT INTO documents(key, kind, session, job, body, sha256, created_by, created_at, updated_at) VALUES($1, 'report', '', '', $2, 'sha1', 'test', $3, $3)`,
		"deploy/handoffkeep/20261112T070000Z", hkDeployDoc, hkDeployTime); err != nil {
		t.Fatal(err)
	}
	pwDeployDoc := fmt.Sprintf(`{"schema":"deploy-record/v0","service":"panewire-hub","result":"success","deployed_at":"%s"}`, pwDeployTime.Format(time.RFC3339))
	if _, err := st.pool.Exec(ctx, `INSERT INTO documents(key, kind, session, job, body, sha256, created_by, created_at, updated_at) VALUES($1, 'report', '', '', $2, 'sha2', 'test', $3, $3)`,
		"deploy/panewire-hub/20261111T120000Z", pwDeployDoc, pwDeployTime); err != nil {
		t.Fatal(err)
	}

	setTask := func(id int64, state string, createdAt time.Time, claimedBy string) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, `UPDATE tasks SET state=$2, created_at=$3, updated_at=$3, claimed_by=$4 WHERE id=$1`, id, state, createdAt, claimedBy); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Seed bundle parent
	bundleParent, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "[bundle test B01] Root bundle parent",
		Kind:      "implement",
		State:     StateBacklog,
		Priority:  10,
		CreatedBy: "test",
		CreatedAt: now.Add(-5 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(bundleParent.ID, StateBacklog, now.Add(-5*time.Hour), "")

	// 2. Seed backlog tasks (one child of bundleParent, one normal task)
	// Higher priority task created later vs lower priority created earlier
	backlog1, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Backlog task low priority older",
		Kind:      "implement",
		State:     StateBacklog,
		Priority:  1,
		CreatedBy: "test",
		CreatedAt: now.Add(-4 * time.Hour),
		Project:   strPtr("handoffkeep"),
		ParentID:  &bundleParent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(backlog1.ID, StateBacklog, now.Add(-4*time.Hour), "")

	backlog2, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Backlog task high priority newer",
		Kind:      "implement",
		State:     StateBacklog,
		Priority:  9,
		CreatedBy: "test",
		CreatedAt: now.Add(-2 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(backlog2.ID, StateBacklog, now.Add(-2*time.Hour), "")

	// 3. Seed in-work tasks (researching, developing, reviewing)
	inWork1, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Developing task with job and PR",
		Kind:      "implement",
		State:     StateDeveloping,
		Priority:  5,
		CreatedBy: "test",
		CreatedAt: now.Add(-10 * time.Hour),
		Project:   strPtr("handoffkeep"),
		Refs: TaskRefs{
			PR:    "https://github.com/mgh3326/handoffkeep/pull/100",
			JobID: "job-dev-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(inWork1.ID, StateDeveloping, now.Add(-10*time.Hour), "")
	// Seed transition event for developing state entry
	devSince := now.Add(-3 * time.Hour)
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: inWork1.ID,
		From:   StateBacklog,
		To:     StateDeveloping,
		By:     "test",
		At:     devSince,
	}); err != nil {
		t.Fatal(err)
	}
	// Seed job record
	if _, _, err := st.ClaimJob(ctx, Job{
		JobID:     "job-dev-1",
		TaskID:    &inWork1.ID,
		Host:      "desktop-worker",
		CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}

	inWork2, err := st.CreateTask(ctx, Task{
		Lane:      "panewire-design",
		Title:     "Reviewing task",
		Kind:      "implement",
		State:     StateReviewing,
		Priority:  3,
		CreatedBy: "test",
		CreatedAt: now.Add(-8 * time.Hour),
		Project:   strPtr("panewire"),
		ClaimedBy: "mac-reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(inWork2.ID, StateReviewing, now.Add(-8*time.Hour), "mac-reviewer")
	revSince := now.Add(-1 * time.Hour)
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: inWork2.ID,
		From:   StateDeveloping,
		To:     StateReviewing,
		By:     "test",
		At:     revSince,
	}); err != nil {
		t.Fatal(err)
	}

	// 4. Seed merged tasks:
	// mergedWaiting: merged AFTER handoffkeep deploy time (2026-11-12T07:00:00Z) -> waiting_deploy
	mergedWaiting, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Merged after deploy: waiting deploy",
		Kind:      "implement",
		State:     StateMerged,
		Priority:  2,
		CreatedBy: "test",
		CreatedAt: now.Add(-12 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(mergedWaiting.ID, StateMerged, now.Add(-12*time.Hour), "")
	mergeAfterTime := hkDeployTime.Add(1 * time.Hour)
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: mergedWaiting.ID,
		From:   StateReviewing,
		To:     StateMerged,
		By:     "test",
		At:     mergeAfterTime,
	}); err != nil {
		t.Fatal(err)
	}

	// mergedBefore: merged BEFORE handoffkeep deploy time -> merged_no_deploy_record (already deployed)
	mergedBefore, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Merged before deploy: already deployed",
		Kind:      "implement",
		State:     StateMerged,
		Priority:  2,
		CreatedBy: "test",
		CreatedAt: now.Add(-24 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(mergedBefore.ID, StateMerged, now.Add(-24*time.Hour), "")
	mergeBeforeTime := hkDeployTime.Add(-2 * time.Hour)
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: mergedBefore.ID,
		From:   StateReviewing,
		To:     StateMerged,
		By:     "test",
		At:     mergeBeforeTime,
	}); err != nil {
		t.Fatal(err)
	}

	// mergedNoRecord: ParkingArcade has no deploy service -> merged_no_deploy_record
	mergedNoRecord, err := st.CreateTask(ctx, Task{
		Lane:      "parkingarcade-design",
		Title:     "Merged in project with no deploy record",
		Kind:      "implement",
		State:     StateMerged,
		Priority:  1,
		CreatedBy: "test",
		CreatedAt: now.Add(-5 * time.Hour),
		Project:   strPtr("ParkingArcade"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(mergedNoRecord.ID, StateMerged, now.Add(-5*time.Hour), "")
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: mergedNoRecord.ID,
		From:   StateReviewing,
		To:     StateMerged,
		By:     "test",
		At:     now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// 5. Seed movement (closed task events across yesterday and today)
	// Completed task today
	closedToday, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Completed today",
		Kind:      "implement",
		State:     StateCompleted,
		CreatedBy: "test",
		CreatedAt: now.Add(-6 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(closedToday.ID, StateCompleted, now.Add(-6*time.Hour), "")
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: closedToday.ID,
		From:   StateDeveloping,
		To:     StateCompleted,
		By:     "test",
		At:     now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Dropped task yesterday (24h ago)
	closedYesterday, err := st.CreateTask(ctx, Task{
		Lane:      "handoffkeep-design",
		Title:     "Dropped yesterday",
		Kind:      "implement",
		State:     StateDropped,
		CreatedBy: "test",
		CreatedAt: now.Add(-30 * time.Hour),
		Project:   strPtr("handoffkeep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	setTask(closedYesterday.ID, StateDropped, now.Add(-30*time.Hour), "")
	if _, _, err := insertEvent(ctx, st.pool, taskEventInput{
		TaskID: closedYesterday.ID,
		From:   StateBacklog,
		To:     StateDropped,
		By:     "test",
		At:     now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// EXECUTE ReadWorkStatus
	status, err := st.ReadWorkStatus(ctx, now)
	if err != nil {
		t.Fatalf("ReadWorkStatus failed: %v", err)
	}

	// VERIFY 1: Counts per project and summary
	// Summary checks:
	// Bundle parent is NOT counted as work.
	// handoffkeep has:
	// - backlog: backlog1, backlog2 (2)
	// - in_work: inWork1 (1)
	// - in_review: 0
	// - waiting_deploy: mergedWaiting (1)
	// - merged_no_deploy_record: mergedBefore (1)
	// - total_open: 2 + 1 + 0 + 1 = 4
	// - bundle_parents: 1
	// panewire has:
	// - backlog: 0, in_work: 0, in_review: 1 (inWork2), waiting_deploy: 0, total_open: 1
	// ParkingArcade has:
	// - backlog: 0, in_work: 0, in_review: 0, waiting_deploy: 0, merged_no_deploy_record: 1, total_open: 0
	// Total open: 4 (hk) + 1 (pw) + 0 (pa) = 5
	if status.Summary.TotalOpen != 5 {
		t.Errorf("got Summary.TotalOpen=%d, want 5", status.Summary.TotalOpen)
	}
	if status.Summary.Backlog != 2 {
		t.Errorf("got Summary.Backlog=%d, want 2", status.Summary.Backlog)
	}
	if status.Summary.InWork != 1 {
		t.Errorf("got Summary.InWork=%d, want 1", status.Summary.InWork)
	}
	if status.Summary.InReview != 1 {
		t.Errorf("got Summary.InReview=%d, want 1", status.Summary.InReview)
	}
	if status.Summary.WaitingDeploy != 1 {
		t.Errorf("got Summary.WaitingDeploy=%d, want 1", status.Summary.WaitingDeploy)
	}
	if status.Summary.MergedNoDeployRecord != 2 {
		t.Errorf("got Summary.MergedNoDeployRecord=%d, want 2", status.Summary.MergedNoDeployRecord)
	}
	if status.Summary.BundleParents != 1 {
		t.Errorf("got Summary.BundleParents=%d, want 1", status.Summary.BundleParents)
	}

	// Project specific checks
	hkFound := false
	for _, p := range status.Projects {
		if p.Project == "handoffkeep" {
			hkFound = true
			if p.Counts.TotalOpen != 4 {
				t.Errorf("handoffkeep TotalOpen=%d, want 4", p.Counts.TotalOpen)
			}
			if p.Counts.Backlog != 2 {
				t.Errorf("handoffkeep Backlog=%d, want 2", p.Counts.Backlog)
			}
			if p.Counts.InWork != 1 {
				t.Errorf("handoffkeep InWork=%d, want 1", p.Counts.InWork)
			}
			if p.Counts.WaitingDeploy != 1 {
				t.Errorf("handoffkeep WaitingDeploy=%d, want 1", p.Counts.WaitingDeploy)
			}
			if p.Counts.MergedNoDeployRecord != 1 {
				t.Errorf("handoffkeep MergedNoDeployRecord=%d, want 1", p.Counts.MergedNoDeployRecord)
			}
			if p.Counts.BundleParents != 1 {
				t.Errorf("handoffkeep BundleParents=%d, want 1", p.Counts.BundleParents)
			}
		}
	}
	if !hkFound {
		t.Fatal("handoffkeep project not found in projects")
	}

	// VERIFY 2: In-work list (with job, host, PR, age)
	if len(status.InWork) != 2 {
		t.Fatalf("got len(InWork)=%d, want 2", len(status.InWork))
	}
	// inWork2 (reviewing, since 1h ago) vs inWork1 (developing, since 3h ago)
	// Ordered by since DESC, so inWork2 should be first
	if status.InWork[0].ID != inWork2.ID {
		t.Errorf("InWork[0].ID=%d, want inWork2 %d", status.InWork[0].ID, inWork2.ID)
	}
	if status.InWork[0].Host != "mac-reviewer" {
		t.Errorf("InWork[0].Host=%q, want 'mac-reviewer'", status.InWork[0].Host)
	}
	if status.InWork[1].ID != inWork1.ID {
		t.Errorf("InWork[1].ID=%d, want inWork1 %d", status.InWork[1].ID, inWork1.ID)
	}
	if status.InWork[1].JobID != "job-dev-1" {
		t.Errorf("InWork[1].JobID=%q, want 'job-dev-1'", status.InWork[1].JobID)
	}
	if status.InWork[1].Host != "desktop-worker" {
		t.Errorf("InWork[1].Host=%q, want 'desktop-worker'", status.InWork[1].Host)
	}
	if status.InWork[1].PR != "https://github.com/mgh3326/handoffkeep/pull/100" {
		t.Errorf("InWork[1].PR=%q, want PR url", status.InWork[1].PR)
	}
	if status.InWork[1].AgeSeconds < 10000 || status.InWork[1].AgeSeconds > 12000 {
		t.Errorf("InWork[1].AgeSeconds=%d, want around 10800s (3h)", status.InWork[1].AgeSeconds)
	}

	// VERIFY 3: Backlog list order (priority DESC, age ASC) and bundle link
	// backlog2 has priority 9 (newer), backlog1 has priority 1 (older)
	// backlog2 must come before backlog1!
	if len(status.Backlog) != 2 {
		t.Fatalf("got len(Backlog)=%d, want 2", len(status.Backlog))
	}
	if status.Backlog[0].ID != backlog2.ID {
		t.Errorf("Backlog[0].ID=%d (priority %d), want backlog2 %d (priority 9)",
			status.Backlog[0].ID, status.Backlog[0].Priority, backlog2.ID)
	}
	if status.Backlog[1].ID != backlog1.ID {
		t.Errorf("Backlog[1].ID=%d (priority %d), want backlog1 %d (priority 1)",
			status.Backlog[1].ID, status.Backlog[1].Priority, backlog1.ID)
	}
	// backlog1 is child of bundleParent -> check bundle link
	if status.Backlog[1].BundleID == nil || *status.Backlog[1].BundleID != bundleParent.ID {
		t.Errorf("Backlog[1].BundleID=%v, want %d", status.Backlog[1].BundleID, bundleParent.ID)
	}
	if status.Backlog[1].BundleTitle != bundleParent.Title {
		t.Errorf("Backlog[1].BundleTitle=%q, want %q", status.Backlog[1].BundleTitle, bundleParent.Title)
	}

	// VERIFY 4: Movement (today and 7 prior days = 8 days in KST)
	if len(status.Movement) != 8 {
		t.Fatalf("got len(Movement)=%d, want 8", len(status.Movement))
	}
	todayStr := now.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02")
	if status.Movement[7].Date != todayStr {
		t.Errorf("Movement[7].Date=%q, want today %q", status.Movement[7].Date, todayStr)
	}
	if status.Movement[7].Closed < 1 {
		t.Errorf("Movement[7].Closed=%d, want at least 1 (closedToday)", status.Movement[7].Closed)
	}
	if status.Movement[7].Created < 1 {
		t.Errorf("Movement[7].Created=%d, want at least 1", status.Movement[7].Created)
	}

	// VERIFY 5: Deploy-waiting rule
	// mergedWaiting was counted as WaitingDeploy (1)
	// mergedBefore was counted as MergedNoDeployRecord (1)
	// mergedNoRecord (ParkingArcade) was counted as MergedNoDeployRecord (1)
	// Already confirmed in Summary and Projects checks above!
}

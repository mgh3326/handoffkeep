package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestModelConstraintsAndHistory(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	p, err := s.SetProjectMetadata(ctx, ProjectMetadata{Name: "handoffkeep", Status: "active", StartAt: &start, TargetAt: &end})
	if err != nil || p.Status != "active" {
		t.Fatalf("project metadata=%+v err=%v", p, err)
	}
	if _, err = s.SetProjectMetadata(ctx, ProjectMetadata{Name: "handoffkeep", StartAt: &end, TargetAt: &start}); err == nil {
		t.Fatal("accepted reversed project dates")
	}
	if _, err = s.SetProjectMetadata(ctx, ProjectMetadata{Name: "missing"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: "model-test", Title: "model", Kind: "implement", CreatedBy: "test", Project: projectPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.SaveMilestone(ctx, ProjectMilestone{Project: "wrk", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskMilestone(ctx, x.ID, &m.ID, "test"); !errors.Is(err, ErrMilestoneProject) {
		t.Fatalf("cross-project milestone: %v", err)
	}
	m, err = s.SaveMilestone(ctx, ProjectMilestone{Project: "handoffkeep", Name: "same"})
	if err != nil {
		t.Fatal(err)
	}
	updatedDescription := "independent change"
	if _, err = s.PatchMilestone(ctx, m.ID, MilestonePatch{Description: &updatedDescription}); err != nil {
		t.Fatal(err)
	}
	newName := "renamed"
	patched, err := s.PatchMilestone(ctx, m.ID, MilestonePatch{Name: &newName})
	if err != nil || patched.Description != updatedDescription || patched.Name != newName {
		t.Fatalf("partial milestone update=%+v err=%v", patched, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	concurrentName, concurrentStatus := "concurrent-name", "concurrent-status"
	for _, patch := range []MilestonePatch{{Name: &concurrentName}, {Status: &concurrentStatus}} {
		wg.Add(1)
		go func(p MilestonePatch) { defer wg.Done(); _, err := s.PatchMilestone(ctx, m.ID, p); errs <- err }(patch)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	patched, err = s.GetMilestone(ctx, m.ID)
	if err != nil || patched.Name != concurrentName || patched.Status != concurrentStatus {
		t.Fatalf("concurrent updates=%+v err=%v", patched, err)
	}
	if _, changed, err := s.SetTaskMilestone(ctx, x.ID, &m.ID, "test"); err != nil || !changed {
		t.Fatalf("set milestone changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskMilestone(ctx, x.ID, &m.ID, "test"); err != nil || changed {
		t.Fatalf("repeat milestone changed=%t err=%v", changed, err)
	}
	status := "changed"
	before, err := s.GetProjectMetadata(ctx, "wrk")
	if err != nil {
		t.Fatal(err)
	}
	patch := &ProjectMetadataPatch{Status: &status}
	if _, _, err = s.SetTaskProjectWithMetadata(ctx, x.ID, "wrk", "test", "", patch); !errors.Is(err, ErrMilestoneProject) {
		t.Fatalf("move with milestone: %v", err)
	}
	if _, _, err = s.SetTaskProjectWithMetadata(ctx, x.ID+999999, "wrk", "test", "", patch); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("move missing: %v", err)
	}
	after, err := s.GetProjectMetadata(ctx, "wrk")
	if err != nil || after.Status != before.Status {
		t.Fatalf("refused move changed project: before=%+v after=%+v err=%v", before, after, err)
	}
	if _, err = s.PatchProjectMetadata(ctx, "wrk", ProjectMetadataPatch{StartAt: &start}); err == nil {
		t.Fatal("accepted start without set_start")
	}
	plain, err := s.SaveLabel(ctx, Label{Name: "plain", EditLevel: "anyone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveLabel(ctx, Label{Name: "bad-parent", ParentID: &plain.ID, EditLevel: "anyone"}); !errors.Is(err, ErrLabelParent) {
		t.Fatalf("non-group parent: %v", err)
	}
	if _, err = s.SaveLabel(ctx, Label{Name: "bad-level", EditLevel: "unknown"}); err == nil {
		t.Fatal("accepted invalid edit level")
	}
	group, err := s.SaveLabel(ctx, Label{Name: "group", IsGroup: true, EditLevel: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.SaveLabel(ctx, Label{Name: "child", IsGroup: true, ParentID: &group.ID, EditLevel: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	group.ParentID = &child.ID
	if _, err = s.SaveLabel(ctx, group); !errors.Is(err, ErrLabelCycle) {
		t.Fatalf("cycle: %v", err)
	}
	if _, changed, err := s.SetTaskLabel(ctx, x.ID, plain.ID, "test", true); err != nil || !changed {
		t.Fatalf("attach changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskLabel(ctx, x.ID, plain.ID, "test", true); err != nil || changed {
		t.Fatalf("repeat attach changed=%t err=%v", changed, err)
	}
	if err = s.RetireLabel(ctx, plain.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskLabel(ctx, x.ID, plain.ID, "test", true); !errors.Is(err, ErrLabelRetired) {
		t.Fatalf("retired attach: %v", err)
	}
	for i, active := range []bool{true, true, false, false} {
		_, changed, err := s.SetTaskSubscriber(ctx, x.ID, "person", "test", "test", active)
		if err != nil {
			t.Fatal(err)
		}
		if changed != (i == 0 || i == 2) {
			t.Fatalf("subscription attempt %d changed=%t", i, changed)
		}
	}
	var activeCount, allCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE archived_at IS NULL),count(*) FROM task_subscribers WHERE task_id=$1 AND subscriber='person'`, x.ID).Scan(&activeCount, &allCount); err != nil || activeCount != 0 || allCount != 1 {
		t.Fatalf("subscriptions active=%d all=%d err=%v", activeCount, allCount, err)
	}
	rows, err := pool.Query(ctx, `SELECT from_value,to_value FROM task_events WHERE task_id=$1 AND field='subscribers' ORDER BY id`, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	var subscriptionHistory [][2]string
	for rows.Next() {
		var from, to string
		if err = rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		subscriptionHistory = append(subscriptionHistory, [2]string{from, to})
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(subscriptionHistory) != 2 || subscriptionHistory[0] != [2]string{"", "person"} || subscriptionHistory[1] != [2]string{"person", ""} {
		t.Fatalf("subscription history=%v", subscriptionHistory)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	high, breach := now.Add(2*time.Hour), now.Add(3*time.Hour)
	d := TaskDeadlines{DueAt: &later, SLAStartedAt: &now, SLAMediumRiskAt: &later, SLAHighRiskAt: &high, SLABreachesAt: &breach}
	if _, changed, err := s.SetTaskDeadlines(ctx, x.ID, d, "test"); err != nil || !changed {
		t.Fatalf("deadlines changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskDeadlines(ctx, x.ID, d, "test"); err != nil || changed {
		t.Fatalf("repeat deadlines changed=%t err=%v", changed, err)
	}
	for field, want := range map[string]int{"milestone_id": 1, "labels": 1, "subscribers": 2, "due_at": 1, "sla_started_at": 1, "sla_medium_risk_at": 1, "sla_high_risk_at": 1, "sla_breaches_at": 1} {
		var n int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND field=$2`, x.ID, field).Scan(&n); err != nil || n != want {
			t.Errorf("history %s count=%d want=%d err=%v", field, n, want, err)
		}
	}
	for field, value := range map[string]string{"milestone_id": fmt.Sprint(m.ID), "labels": fmt.Sprint(plain.ID), "due_at": deadlineText(&later), "sla_started_at": deadlineText(&now), "sla_medium_risk_at": deadlineText(&later), "sla_high_risk_at": deadlineText(&high), "sla_breaches_at": deadlineText(&breach)} {
		var from, to string
		if err = pool.QueryRow(ctx, `SELECT from_value,to_value FROM task_events WHERE task_id=$1 AND field=$2`, x.ID, field).Scan(&from, &to); err != nil || from != "" || to != value {
			t.Errorf("history %s: %q -> %q want %q err=%v", field, from, to, value, err)
		}
	}
	if _, _, err = s.SetTaskDeadlines(ctx, x.ID, TaskDeadlines{SLAStartedAt: &high, SLABreachesAt: &now}, "test"); err == nil {
		t.Fatal("accepted reversed SLA times")
	}
	if _, changed, err := s.SetTaskMilestone(ctx, x.ID, nil, "test"); err != nil || !changed {
		t.Fatalf("clear milestone changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskLabel(ctx, x.ID, plain.ID, "test", false); err != nil || !changed {
		t.Fatalf("detach retired label changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskDeadlines(ctx, x.ID, TaskDeadlines{}, "test"); err != nil || !changed {
		t.Fatalf("clear deadlines changed=%t err=%v", changed, err)
	}
	if _, changed, err := s.SetTaskDeadlines(ctx, x.ID, TaskDeadlines{}, "test"); err != nil || changed {
		t.Fatalf("repeat clear changed=%t err=%v", changed, err)
	}
}

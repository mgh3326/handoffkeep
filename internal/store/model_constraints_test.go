package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestModelConstraintsAndHistory(t *testing.T) {
	s, _ := searchTestStore(t)
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
}

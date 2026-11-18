package store

import (
	"errors"
	"testing"
	"time"
)

// Dialect-neutral cases of the frontier, settle, sweep, drift and hygiene
// reports and the last-activity repair (hk 5074, A3).

func init() {
	registerTaskAreaCase("hygiene/settle-merged", func(t *testing.T, s *Store) {
		ctx := t.Context()
		if _, err := s.SettleMerged(ctx, "bad project!", true); err == nil {
			t.Fatal("SettleMerged accepted an invalid project name")
		}
		for _, dry := range []bool{true, false} {
			for _, project := range []string{"", "no-such-project"} {
				rep, err := s.SettleMerged(ctx, project, dry)
				if err != nil {
					t.Fatalf("SettleMerged(%q, dry=%v): %v", project, dry, err)
				}
				if rep.DryRun != dry || rep.Project != project || len(rep.Counts) != len(settleReasons) || len(rep.Tasks) != len(settleReasons) {
					t.Fatalf("SettleMerged(%q, dry=%v) = %+v", project, dry, rep)
				}
				for _, reason := range settleReasons {
					if rep.Counts[reason] != 0 || rep.Tasks[reason] == nil || len(rep.Tasks[reason]) != 0 {
						t.Fatalf("reason %s on a store without merged tasks = %d %v", reason, rep.Counts[reason], rep.Tasks[reason])
					}
				}
			}
		}
	})

	registerTaskAreaCase("hygiene/sweep", func(t *testing.T, s *Store) {
		ctx := t.Context()
		for _, days := range []int{-1, MaxBacklogIceboxDays + 1} {
			if _, err := s.SweepBacklog(ctx, days, true); err == nil {
				t.Fatalf("SweepBacklog accepted %d days", days)
			}
		}
		off, err := s.SweepBacklog(ctx, 0, false)
		if err != nil || off == nil || len(off) != 0 {
			t.Fatalf("SweepBacklog(0) = %+v err=%v, want the off setting to move nothing", off, err)
		}
		if _, created, err := s.IntakeTask(ctx, IntakeInput{Source: "hygiene-test", SourceRef: "sweep/1", Title: "Triage is never swept", CreatedBy: "tester"}); err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		project := "hygiene-sweep"
		_, err = s.AddTaskProject(ctx, project, "tester")
		if err != nil {
			t.Fatalf("AddTaskProject: %v", err)
		}
		_, err = s.CreateTask(ctx, Task{Lane: "hygiene-sweep", Title: "Fresh backlog task", Kind: "implement", CreatedBy: "tester", Project: &project})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		for _, dry := range []bool{true, false} {
			moved, err := s.SweepBacklog(ctx, 1, dry)
			if err != nil || len(moved) != 0 {
				t.Fatalf("SweepBacklog(1, dry=%v) = %+v err=%v, want a fresh task left alone", dry, moved, err)
			}
		}
	})

	registerTaskAreaCase("hygiene/repair-last-activity", func(t *testing.T, s *Store) {
		ctx := t.Context()
		if _, created, err := s.IntakeTask(ctx, IntakeInput{Source: "hygiene-test", SourceRef: "repair/1", Title: "Repair candidate", CreatedBy: "tester"}); err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		n, err := s.RepairLastActivity(ctx, 100)
		if err != nil || n != 0 {
			t.Fatalf("RepairLastActivity = %d err=%v, want 0 on a consistent store", n, err)
		}
	})

	registerTaskAreaCase("hygiene/frontier", func(t *testing.T, s *Store) {
		ctx := t.Context()
		now := time.Now().UTC()
		if _, err := s.ProjectFrontier(ctx, "no-such-project", now.Add(-time.Hour), now); !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("frontier of an unknown project err = %v, want ErrProjectNotFound", err)
		}
		project := "hygiene-frontier"
		_, err := s.AddTaskProject(ctx, project, "tester")
		if err != nil {
			t.Fatalf("AddTaskProject: %v", err)
		}
		f, err := s.ProjectFrontier(ctx, project, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		if f.Project != project || f.Truncated || f.Counts != (FrontierCounts{}) || f.Changes == nil || len(f.Changes) != 0 || f.ChangedSince.Count != 0 || f.Gates != (FrontierGates{}) {
			t.Fatalf("frontier of an empty project = %+v", f)
		}
	})

	registerTaskAreaCase("hygiene/drift", func(t *testing.T, s *Store) {
		ctx := t.Context()
		now := time.Now().UTC()
		for _, days := range []int{0, MaxDriftMergedDays + 1} {
			if _, err := s.TaskDrift(ctx, now, days); err == nil {
				t.Fatalf("TaskDrift accepted merged_days %d", days)
			}
		}
		d, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]TaskDriftClass{
			"settled_open_decision": d.SettledOpenDecision, "merged_pr_not_merged": d.MergedPRNotMerged,
			"auto_close_held": d.AutoCloseHeld, "merged_without_deploy": d.MergedWithoutDeploy, "idle_started": d.IdleStarted,
			"explicit_all_settled": d.ExplicitAllSettled,
		} {
			if c.Rule == "" || c.Count != 0 || c.Truncated || c.Tasks == nil || len(c.Tasks) != 0 {
				t.Fatalf("drift class %s on an empty store = %+v", name, c)
			}
		}
		if d.MergedDays != DriftDefaultMergedDays || d.StartedStalledHours != DefaultStartedStalledHours {
			t.Fatalf("drift settings = %+v", d)
		}
	})

	registerTaskAreaCase("hygiene/report", func(t *testing.T, s *Store) {
		ctx := t.Context()
		now := time.Now().UTC()
		if _, err := s.TaskHygiene(ctx, now, "bad project!", true); err == nil {
			t.Fatal("TaskHygiene accepted an invalid project name")
		}
		for _, dry := range []bool{true, false} {
			rep, err := s.TaskHygiene(ctx, now, "", dry)
			if err != nil {
				t.Fatalf("TaskHygiene(dry=%v): %v", dry, err)
			}
			f := rep.Findings
			if rep.DryRun != dry || rep.MechanicalCloses == nil || len(rep.MechanicalCloses) != 0 || rep.SettleMerged == nil ||
				len(f.MergedPROpenTasks) != 0 || len(f.AutoCloseHeldParents) != 0 || len(f.SettledOpenDecisions) != 0 ||
				len(f.LiveAttemptsWithoutLease) != 0 || len(f.AnsweredDecisionsNotDelivered) != 0 || len(f.MergedWithoutDeploy) != 0 {
				t.Fatalf("TaskHygiene(dry=%v) on an empty store = %+v", dry, rep)
			}
		}
	})
}

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This assertion kills a mutant that removes the reason guard, even without
// a database: a missing reason must be rejected before any storage access.
func TestReopenReasonRequired(t *testing.T) {
	for _, reason := range []string{"", " ", "\n\t"} {
		_, err := (*Store)(nil).ReopenTask(context.Background(), 1, "tester", reason)
		if err == nil || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
}

func TestReopenClearsCycleRefs(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	old := TaskRefs{PR: "https://github.com/example/project/pull/1", HeadSHA: strings.Repeat("a", 40), ReportPath: "jobs/old-job/report.md", JobID: "old-job", OriginTask: 123}
	for _, terminal := range []string{"merged", "dropped"} {
		t.Run(terminal, func(t *testing.T) {
			x, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "reopen-refs"), Title: "old cycle", Kind: "fix", Refs: old, CreatedBy: "tester", Project: projectPtr(testProjectName)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimTask(ctx, x.ID, "old-builder", old.JobID, ""); err != nil {
				t.Fatal(err)
			}
			steps := []string{"dropped"}
			if terminal == "merged" {
				steps = []string{"in_progress", "join", "merged"}
			}
			for _, to := range steps {
				if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "old cycle", nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			before, found, err := s.GetTask(ctx, x.ID)
			if err != nil || !found {
				t.Fatalf("before found=%v err=%v", found, err)
			}
			got, err := s.ReopenTask(ctx, x.ID, "tester", "reverted")
			if err != nil {
				t.Fatal(err)
			}
			want := TaskRefs{OriginTask: old.OriginTask}
			if got.ClaimedBy != "" || !reflect.DeepEqual(got.Refs, want) {
				t.Fatalf("stale cycle refs after reopen: claimant=%q refs=%+v", got.ClaimedBy, got.Refs)
			}
			got, found, err = s.GetTask(ctx, x.ID)
			if err != nil || !found || len(got.Events) != len(before.Events)+1 {
				t.Fatalf("after found=%v events=%d err=%v", found, len(got.Events), err)
			}
			if !reflect.DeepEqual(got.Events[:len(before.Events)], before.Events) || !reflect.DeepEqual(got.Events[len(got.Events)-1].Refs, &want) {
				t.Fatal("reopen changed historical snapshots or retained cycle refs in its event")
			}
			if _, err := s.ClaimTask(ctx, x.ID, "new-builder", "new-job", ""); err != nil {
				t.Fatal(err)
			}
			for _, to := range []string{"in_progress", "join", "merged"} {
				got, err = s.TransitionTask(ctx, x.ID, to, "tester", "new cycle without a PR", nil, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if got.Refs.PR != "" || got.Refs.HeadSHA != "" || got.Refs.ReportPath != "" || got.Refs.JobID != "new-job" {
				t.Fatalf("new merge inherited old cycle refs: %+v", got.Refs)
			}
		})
	}
}

func TestReopenRefusesDisposition(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, key := range []string{"A", "E"} {
		merged := time.Now().UTC().Add(-time.Hour)
		x, created, err := s.CreateDisposition(ctx, DispositionInput{
			Lane: relaneLane(t, "reopen-disposition"), Project: testProjectName, Title: "applied disposition",
			OriginPR: "https://github.com/example/project/pull/" + map[string]string{"A": "1", "E": "2"}[key],
			MergeSHA: strings.Repeat("a", 40), MergedAt: &merged, Install: DispositionInstall{State: "unknown"}, Recommended: key, CreatedBy: "tester",
		})
		if err != nil || !created {
			t.Fatalf("create disposition created=%v err=%v", created, err)
		}
		x, _, err = s.GetTask(ctx, x.ID)
		if err != nil || len(x.Events) == 0 {
			t.Fatalf("disposition events=%v err=%v", x.Events, err)
		}
		_, err = s.AnswerDisposition(ctx, DispositionAnswerInput{ID: x.ID, Gen: x.Events[len(x.Events)-1].ID, Key: key, OperatorEmail: "tester@example.com", EventID: "answer-" + key})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyDisposition(ctx, x.ID, "tester", "apply"); err != nil {
			t.Fatal(err)
		}
		assertDispositionReopenRefused(t, s, x.ID)
	}
	// Fail closed on key presence, matching NextTask even for a null payload.
	for _, terminal := range []string{"merged", "dropped"} {
		id := seedTaskRow(t, pool, "reopen-null-disposition", "malformed disposition", terminal)
		if _, err := pool.Exec(ctx, `UPDATE tasks SET refs='{"disposition":null}'::jsonb WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		assertDispositionReopenRefused(t, s, id)
	}
}

func assertDispositionReopenRefused(t *testing.T, s *Store, id int64) {
	t.Helper()
	before, _, err := s.GetTask(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenTask(t.Context(), id, "tester", "reverted"); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "disposition items cannot be reopened") {
		t.Fatalf("disposition reopen refusal: %v", err)
	}
	after, _, err := s.GetTask(t.Context(), id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused reopen mutated disposition: err=%v", err)
	}
}

func TestClaimRefusesDisposition(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, state := range []string{"backlog", "reopened", "claimed"} {
		for _, disposition := range []string{`{"schema":"disposition/v1"}`, `null`} {
			id := seedTaskRow(t, pool, "claim-disposition", "defensive claim guard", state)
			// claimed also exercises the replay path: it must refuse before
			// accepting the matching claimant/job as an idempotent claim.
			if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=jsonb_build_object('disposition',$2::jsonb,'job_id','job-cycle'),claimed_by='builder' WHERE id=$1`, id, disposition); err != nil {
				t.Fatal(err)
			}
			before, _, err := s.GetTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimTask(ctx, id, "builder", "job-cycle", ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "disposition items cannot be claimed directly") {
				t.Fatalf("disposition claim refusal (state=%s refs=%s): %v", state, disposition, err)
			}
			after, _, err := s.GetTask(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused claim mutated disposition: err=%v", err)
			}
		}
	}
}

func TestReopenTerminalAuditAndRemerge(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for _, terminal := range []string{"merged", "dropped"} {
		x := relaneTask(t, s, relaneLane(t, "reopen"), "revert recovery")
		if _, err := s.ClaimTask(ctx, x.ID, "builder", "old-job", ""); err != nil {
			t.Fatal(err)
		}
		steps := []string{"dropped"}
		if terminal == "merged" {
			steps = []string{"in_progress", "join", "merged"}
		}
		for _, to := range steps {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "step", nil, ""); err != nil {
				t.Fatal(err)
			}
		}
		for _, to := range []string{"backlog", "claimed", "in_progress", "reopened"} {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "bypass", nil, "recovery"); err == nil {
				t.Fatalf("generic terminal exit to %s accepted", to)
			}
		}
		before, _, _ := s.GetTask(ctx, x.ID)
		for _, reason := range []string{"", " \n"} {
			if _, err := s.ReopenTask(ctx, x.ID, "tester", reason); err == nil {
				t.Fatal("reasonless reopen accepted")
			}
		}
		unchanged, _, _ := s.GetTask(ctx, x.ID)
		if unchanged.State != terminal || len(unchanged.Events) != len(before.Events) {
			t.Fatal("failed reopen mutated history")
		}
		got, err := s.ReopenTask(ctx, x.ID, "tester", "reverted after failed smoke")
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "reopened" || got.ClaimedBy != "" || got.Refs.JobID != "" {
			t.Fatalf("reopened=%+v", got)
		}
		got, _, _ = s.GetTask(ctx, x.ID)
		event := got.Events[len(got.Events)-1]
		if event.Kind != TaskEventTransition || event.From != terminal || event.To != "reopened" || event.By != "tester" || event.Note != "reverted after failed smoke" {
			t.Fatalf("audit=%+v", event)
		}
		if _, err := s.ReopenTask(ctx, x.ID, "tester", "again"); err == nil {
			t.Fatal("open task reopened")
		}
		events, err := s.ListMergedTaskEvents(ctx, 2000)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.TaskID == x.ID {
				t.Fatal("reopened task still shown as merged")
			}
		}
		next, err := s.NextTask(ctx, x.Lane, "new-builder", "new-job", "")
		if err != nil || next.ID != x.ID {
			t.Fatalf("next=%+v err=%v", next, err)
		}
		got, _, _ = s.GetTask(ctx, x.ID)
		if got.Events[len(got.Events)-1].From != "reopened" {
			t.Fatal("claim lost reopened source")
		}
		for _, to := range []string{"in_progress", "join", "merged"} {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "second cycle", &TaskRefs{PR: "example/project#2"}, ""); err != nil {
				t.Fatal(err)
			}
		}
		events, err = s.ListMergedTaskEvents(ctx, 2000)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, e := range events {
			if e.TaskID == x.ID {
				count++
				if e.Refs == nil || e.Refs.PR != "example/project#2" || !e.At.After(event.At) {
					t.Fatalf("latest merge=%+v", e)
				}
			}
		}
		if count != 1 {
			t.Fatalf("merge count=%d", count)
		}
	}
}

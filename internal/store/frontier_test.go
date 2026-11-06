package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestProjectFrontierClasses(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	project := fmt.Sprintf("frontier-%d", time.Now().UnixNano())
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	repo := changeRepo("frontier")
	zero, two := 0, 2
	task := func() Task {
		x, err := s.CreateTask(ctx, Task{Lane: project, Title: "frontier", Kind: "implement", CreatedBy: "tester", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	review := func(n int, x Task, high *int) {
		send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: repoPR(repo, n), TaskID: x.ID, HeadSHA: testSHA1, Reviews: []TaskEventReview{{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", Critical: &zero, High: high}}})
	}
	xs := make([]Task, 7)
	for i := range xs {
		xs[i] = task()
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, i+1), TaskID: xs[i].ID, HeadSHA: testSHA1})
	}
	review(1, xs[0], &zero)
	review(2, xs[1], &zero)
	review(3, xs[2], &two)
	review(4, xs[3], &zero)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 4), TaskID: xs[3].ID, HeadSHA: testSHA2})
	review(5, xs[4], nil)
	review(6, xs[5], &zero)
	held := mustChange(t, s, repo, 2).ID
	if _, err := s.SetChangeMergeFlow(ctx, held, ChangeMergeFlow{HoldReason: "held"}); err != nil {
		t.Fatal(err)
	}
	dep := mustChange(t, s, repo, 1).ID
	if _, err := s.SetChangeMergeFlow(ctx, mustChange(t, s, repo, 6).ID, ChangeMergeFlow{DependsOnChange: &dep, DependsOnState: "merged"}); err != nil {
		t.Fatal(err)
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(repo, 7), TaskID: xs[6].ID})
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state='merged' WHERE id=$1`, xs[6].ID); err != nil {
		t.Fatal(err)
	}
	// Closed and deployed changes are excluded; another project's change is isolated.
	closedTask, deployedTask := task(), task()
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 8), TaskID: closedTask.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(repo, 9), TaskID: deployedTask.ID})
	if _, err := s.pool.Exec(ctx, `UPDATE changes SET state='closed' WHERE repo=$1 AND pr_number=8`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state='deployed' WHERE id=$1`, deployedTask.ID); err != nil {
		t.Fatal(err)
	}
	other := project + "-other"
	if _, err := s.AddTaskProject(ctx, other, "tester"); err != nil {
		t.Fatal(err)
	}
	otherTask, err := s.CreateTask(ctx, Task{Lane: other, Title: "other", Kind: "implement", CreatedBy: "tester", Project: &other})
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 10), TaskID: otherTask.ID})
	now := time.Now().UTC()
	f, err := s.ProjectFrontier(ctx, project, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Changes) != 7 {
		t.Fatalf("changes %v", f.Changes)
	}
	want := map[int]struct {
		class   string
		reasons []string
	}{1: {"landable", []string{}}, 2: {"blocked", []string{"hold:held"}}, 3: {"blocked", []string{"findings:c0h2"}}, 4: {"waiting", []string{"review"}}, 5: {"waiting", []string{"review"}}, 6: {"waiting", []string{fmt.Sprintf("change:%d:merged", dep)}}, 7: {"verifying", []string{}}}
	for _, x := range f.Changes {
		w := want[x.PRNumber]
		if x.Class != w.class || !reflect.DeepEqual(x.Reasons, w.reasons) {
			t.Errorf("%d: %s %v want %s %v", x.PRNumber, x.Class, x.Reasons, w.class, w.reasons)
		}
	}
}

func TestProjectFrontierSharedAndTruncated(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	p := fmt.Sprintf("frontier-%d", time.Now().UnixNano())
	other := p + "-other"
	for _, v := range []string{p, other} {
		if _, err := s.AddTaskProject(ctx, v, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	makeTask := func(project string) Task {
		x, err := s.CreateTask(ctx, Task{Lane: p, Title: "frontier", Kind: "implement", CreatedBy: "tester", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	a, b := makeTask(p), makeTask(other)
	repo := changeRepo("limit")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 1; i <= 501; i++ {
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, i), TaskID: a.ID, HeadSHA: testSHA1, At: &at})
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 1), TaskID: b.ID, HeadSHA: testSHA1, At: &at})
	now := time.Now().UTC()
	f, err := s.ProjectFrontier(ctx, p, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Truncated || len(f.Changes) != 500 || f.Counts.Total != 500 {
		t.Fatalf("truncated=%v count=%d total=%d", f.Truncated, len(f.Changes), f.Counts.Total)
	}
	for i, x := range f.Changes {
		if x.PRNumber != i+1 {
			t.Fatalf("item %d is PR %d", i, x.PRNumber)
		}
	}
	f, err = s.ProjectFrontier(ctx, other, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Truncated || f.Counts.Total != 1 || len(f.Changes) != 1 || f.Changes[0].PRNumber != 1 {
		t.Fatalf("other: %+v", f)
	}
}

func TestProjectFrontierReviewSince(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	p := fmt.Sprintf("frontier-%d", time.Now().UnixNano())
	if _, err := s.AddTaskProject(ctx, p, "tester"); err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: p, Title: "frontier", Kind: "implement", CreatedBy: "tester", Project: &p})
	if err != nil {
		t.Fatal(err)
	}
	repo := changeRepo("since")
	pr := repoPR(repo, 1)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA1})
	untouched := repoPR(repo, 2)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: untouched, TaskID: x.ID, HeadSHA: testSHA1})
	since := time.Now().UTC()
	zero := 0
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA1, Reviews: []TaskEventReview{{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", Critical: &zero, High: &zero}}})
	f, err := s.ProjectFrontier(ctx, p, since, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if f.ChangedSince.Count != 1 || len(f.ChangedSince.Items) != 1 || f.ChangedSince.Items[0].What != "reviewed" {
		t.Fatalf("changed: %+v", f.ChangedSince)
	}
}

func TestProjectFrontierDecisionGates(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	p := fmt.Sprintf("frontier-%d", time.Now().UnixNano())
	if _, err := s.AddTaskProject(ctx, p, "tester"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		x, err := s.CreateTask(ctx, Task{Lane: p, Title: fmt.Sprintf("decision %d", i), Kind: "implement", CreatedBy: "tester", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "question", nil, ""); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			rows := decisionRowsWhere(t, s, `task_id=$1`, x.ID)
			if len(rows) != 1 {
				t.Fatalf("rows: %v", rows)
			}
			now := time.Now().UTC()
			if _, err = s.SnoozeDecisionRecord(ctx, rows[0].ID, now.Add(time.Hour), now); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UTC()
	f, err := s.ProjectFrontier(ctx, p, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Gates.DecisionsOpen != 1 || len(f.Changes) != 0 {
		t.Fatalf("gates=%+v changes=%v", f.Gates, f.Changes)
	}
}

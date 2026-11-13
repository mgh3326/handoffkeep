package store

import (
	"reflect"
	"testing"
)

func TestResearchTaskKindLifecycle(t *testing.T) {
	s := schemaStore(t, "research_kind")
	task, err := s.CreateTask(t.Context(), Task{
		Lane: relaneLane(t, "research_kind"), Title: "Research request", Kind: "research",
		CreatedBy: "tester", Project: projectPtr(testProjectName),
	})
	if err != nil {
		t.Fatalf("CreateTask(research): %v", err)
	}
	started, err := s.ClaimTask(t.Context(), task.ID, "researcher", "research-kind-job", "")
	if err != nil || started.State != StateResearching {
		t.Fatalf("ClaimTask state=%q err=%v, want researching", started.State, err)
	}
	completed, err := s.TransitionTaskWith(t.Context(), task.ID, TransitionInput{
		To: StateCompleted, By: "researcher", Evidence: "hk:doc findings/research-kind",
	})
	if err != nil || completed.State != StateCompleted {
		t.Fatalf("complete research task state=%q err=%v", completed.State, err)
	}
}

func TestTaskKindsIncludesResearch(t *testing.T) {
	want := []string{"decide", "fix", "implement", "ops", "research", "verify"}
	if got := TaskKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskKinds()=%v, want %v", got, want)
	}
}

func TestMergewakeMergeCompletesResearchTask(t *testing.T) {
	s := schemaStore(t, "research_merge")
	task := flowTask(t, s, s.pool, "research", StateReviewing, true)
	merged := send(t, s, mergeEvent(task.ID, "https://github.com/example/repo/pull/42"))
	if !merged.Applied || merged.Task.State != StateCompleted || merged.Task.Refs.Closure == nil || merged.Task.Refs.Closure.Type != "resolved" {
		t.Fatalf("merge result=%+v, want completed research task with resolved closure", merged)
	}
}

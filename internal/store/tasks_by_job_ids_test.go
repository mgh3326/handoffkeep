package store

import (
	"fmt"
	"testing"
	"time"
)

func TestTasksByJobIDs(t *testing.T) {
	s := linearTestStore(t)
	prefix := fmt.Sprintf("jobs-%d", time.Now().UnixNano())
	var created []Task
	for i, job := range []string{prefix + "-a", prefix + "-b", prefix + "-a", prefix + "-other", ""} {
		task, err := s.CreateTask(t.Context(), Task{Lane: prefix, Title: fmt.Sprintf("task %d", i), Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName), Refs: TaskRefs{JobID: job}})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, task)
	}
	for _, ids := range [][]string{{prefix + "-b", prefix + "-a", prefix + "-a"}, {}, nil} {
		got, err := s.TasksByJobIDs(t.Context(), ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			if len(got) != 0 {
				t.Fatalf("empty IDs returned %+v", got)
			}
			continue
		}
		if len(got) != 3 || got[0].ID != created[0].ID || got[1].ID != created[1].ID || got[2].ID != created[2].ID {
			t.Fatalf("got %+v", got)
		}
	}
}

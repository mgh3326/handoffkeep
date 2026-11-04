package tests

import (
	"fmt"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestTaskEventsThroughRemote drives POST /v1/task-events through the remote
// client against a real server: applied, redelivered, not applied, unknown
// source and missing task.
func TestTaskEventsThroughRemote(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "event target", 0)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	ctx := t.Context()
	pr := fmt.Sprintf("https://github.com/example/app/pull/%d", task.ID)

	res, err := c.ApplyTaskEvent(ctx, store.TaskSourceEvent{Source: store.SourceMergewakeMerge, SourceRef: pr, TaskID: task.ID})
	if err != nil || !res.Applied || res.Redelivered || res.State != store.StateMerged || res.Task.Refs.PR != pr {
		t.Fatalf("applied: %+v err=%v", res, err)
	}
	res, err = c.ApplyTaskEvent(ctx, store.TaskSourceEvent{Source: store.SourceMergewakeMerge, SourceRef: pr, TaskID: task.ID})
	if err != nil || res.Applied || !res.Redelivered || res.State != store.StateMerged {
		t.Fatalf("redelivered: %+v err=%v", res, err)
	}
	res, err = c.ApplyTaskEvent(ctx, store.TaskSourceEvent{Source: store.SourceMergewakePR, SourceRef: pr, TaskID: task.ID})
	if err != nil || res.Applied || res.Redelivered || res.State != store.StateMerged || res.Reason == "" {
		t.Fatalf("not applied: %+v err=%v", res, err)
	}
	if _, err = c.ApplyTaskEvent(ctx, store.TaskSourceEvent{Source: "mergewake.closed", SourceRef: pr, TaskID: task.ID}); remote.ErrorCode(err) != "invalid_context" {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err = c.ApplyTaskEvent(ctx, store.TaskSourceEvent{Source: store.SourceMergewakeMerge, SourceRef: pr, TaskID: 999999999}); remote.ErrorCode(err) != "not_found" {
		t.Fatalf("missing task: %v", err)
	}
}

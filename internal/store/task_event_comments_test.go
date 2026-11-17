package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

func eventComments(t *testing.T, s *Store, task int64) []TaskComment {
	t.Helper()
	comments, err := s.ListTaskComments(t.Context(), task, 0, TaskCommentListMax)
	if err != nil {
		t.Fatal(err)
	}
	return comments
}

func TestTaskEventCommentsAreIdempotentAndReachYouTrackComments(t *testing.T) {
	s, pool := searchTestStore(t)
	_ = pool
	ytbMapped(t, s)
	taskID := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(taskID))
	s.SetYTCommentsMode(YTCommentsOn)
	pr := testPR(4730)

	opened := TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: taskID}
	send(t, s, opened)
	comments := eventComments(t, s, taskID)
	if len(comments) != 1 || comments[0].Body != "PR #4730 opened: "+pr || comments[0].Author != YTCommentPrincipal {
		t.Fatalf("opened comments: %+v", comments)
	}
	op, ok := ytcOpFor(t, s, comments[0].ID)
	if !ok || op.Kind != "comment_append" {
		t.Fatalf("opened comment has no comments-kind op: %+v found=%v", op, ok)
	}

	mergeSHA := strings.Repeat("1", 40)
	merge := TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: taskID, MergeSHA: mergeSHA}
	send(t, s, merge)
	send(t, s, merge)
	comments = eventComments(t, s, taskID)
	if len(comments) != 2 || comments[1].Body != "Merged: PR #4730 (1111111)" {
		t.Fatalf("merge comments: %+v", comments)
	}

	revert := revertEvent(taskID, pr, testPR(5730))
	send(t, s, revert)
	send(t, s, revert)
	comments = eventComments(t, s, taskID)
	if len(comments) != 3 || comments[2].Body != "Reverted: PR #4730" {
		t.Fatalf("revert comments: %+v", comments)
	}
}

func TestR12DeployEventCommentDeduplicatesByTaskAndSHA(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	pr := testPR(4731)
	send(t, s, mergeEvent(task.ID, pr))
	sha := strings.Repeat("a", 40)
	for _, service := range []string{"api", "worker"} {
		key := "deploy/" + service + "/20261115T010000Z"
		body := `{"schema":"deploy-record/v0","service":"` + service + `","target":"prod","result":"success","head_sha":"` + sha + `","deployed_ref":"release-1","included_prs":["` + pr + `"],"source":"test"}`
		if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "report", Session: "deploy-test", Body: body, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	comments := eventComments(t, s, task.ID)
	if len(comments) != 2 || comments[1].Body != "Deployed aaaaaaa (release-1) on api" {
		t.Fatalf("deploy comments: %+v", comments)
	}
}

func TestDeployCommentWithoutHeadSHADeduplicatesAcrossServices(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	pr := testPR(4740)
	send(t, s, mergeEvent(task.ID, pr))
	put := func(key, service string) {
		t.Helper()
		body := `{"schema":"deploy-record/v0","service":"` + service + `","target":"prod","result":"success","deployed_ref":"v1.2","included_prs":["` + pr + `"],"source":"test"}`
		if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "report", Session: "deploy-test", Body: body, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	mergeBack := func() {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='merged' WHERE id=$1`, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	put("deploy/api/20261115T010000Z", "api")
	comments := eventComments(t, s, task.ID)
	if len(comments) != 2 || comments[1].Body != "Deployed unknown (v1.2) on api" {
		t.Fatalf("first deploy comments: %+v", comments)
	}
	// The second service's record for the same deploy applies on a later
	// merged entry; its comment key is the shared deployed ref, so it adds
	// nothing.
	mergeBack()
	put("deploy/worker/20261115T020000Z", "worker")
	mergeBack()
	put("deploy/api/20261115T010000Z", "api")
	comments = eventComments(t, s, task.ID)
	if len(comments) != 2 || comments[1].Body != "Deployed unknown (v1.2) on api" {
		t.Fatalf("deploy comments after second service and replay: %+v", comments)
	}
}

func TestAdoptedPRCommentsOrderMergeBeforeDeploy(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateMerged, false)
	child := flowTask(t, s, pool, "implement", StateDeveloping, true)
	setParent(t, s, child, parent)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='deployed' WHERE id=$1`, child.ID); err != nil {
		t.Fatal(err)
	}
	pr := testPR(4741)
	sha := strings.Repeat("c", 40)
	body := `{"schema":"deploy-record/v0","service":"api","target":"prod","result":"success","head_sha":"` + sha + `","deployed_ref":"release-3","included_prs":["` + pr + `"],"source":"test"}`
	if _, _, err := s.PutDocument(t.Context(), Document{Key: "deploy/api/20261115T030000Z", Kind: "report", Session: "deploy-test", Body: body, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	res := send(t, s, mergeEvent(parent.ID, pr))
	if !res.Applied || res.State != StateDeployed {
		t.Fatalf("adoption with an earlier deploy record: %+v", res)
	}
	comments := eventComments(t, s, parent.ID)
	if len(comments) != 2 || comments[0].Body != "Merged: PR #4741" || comments[1].Body != "Deployed ccccccc (release-3) on api" {
		t.Fatalf("adoption comments out of order: %+v", comments)
	}
}

func TestR12DeployRecordedBeforeMergeCommentsWhenMergeArrives(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	pr := testPR(4734)
	sha := strings.Repeat("b", 40)
	body := `{"schema":"deploy-record/v0","service":"api","target":"prod","result":"success","head_sha":"` + sha + `","deployed_ref":"release-2","included_prs":["` + pr + `"],"source":"test"}`
	if _, _, err := s.PutDocument(t.Context(), Document{Key: "deploy/api/20261115T020000Z", Kind: "report", Session: "deploy-test", Body: body, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	res := send(t, s, mergeEvent(task.ID, pr))
	if res.State != StateDeployed {
		t.Fatalf("merge with an earlier deploy record left state %s", res.State)
	}
	comments := eventComments(t, s, task.ID)
	if len(comments) != 2 || comments[0].Body != "Merged: PR #4734" || comments[1].Body != "Deployed bbbbbbb (release-2) on api" {
		t.Fatalf("deploy-before-merge comments: %+v", comments)
	}
}

func TestTaskEventCommentRefusalDoesNotFailEvent(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	ev := testEvent(SourceMergewakePR, 4732)
	ev.CommentBody = "please include " + "AKIA" + "ABCDEFGHIJKLMNOP"
	res := applyEvent(t, s, task.ID, ev)
	if !res.Applied || len(sourceRows(t, s, task.ID, SourceMergewakePR)) != 1 {
		t.Fatalf("event failed with comment refusal: %+v", res)
	}
	if got := eventComments(t, s, task.ID); len(got) != 0 {
		t.Fatalf("secret-like comment was stored: %+v", got)
	}
}

func TestSystemEventCommentsHaveSeparateKeysAndDeduplicate(t *testing.T) {
	s, _ := searchTestStore(t)
	ytbMapped(t, s)
	taskID := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(taskID))
	s.SetYTCommentsMode(YTCommentsOn)

	const source, eventKey = "hk.deploy", "deploy-record-1"
	h := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", taskID, source, eventKey)))
	mcpKey := "event:" + hex.EncodeToString(h[:])
	if _, _, err := s.CreateMCPTaskComment(t.Context(), taskID, "MCP note", MCPWrite{
		Tool: "test-tool", Actor: "test-actor", Channel: "test", Author: "mcp", Key: mcpKey,
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for range 2 {
		if err = s.insertSystemTaskCommentTx(t.Context(), pgTransaction{tx}, taskID, source, eventKey, "Deployed abc1234 on api", now); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	comments := eventComments(t, s, taskID)
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want one MCP and one event comment: %+v", len(comments), comments)
	}
	var systemRows int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM task_comments WHERE task_id=$1 AND system_event_key IS NOT NULL`, taskID).Scan(&systemRows); err != nil || systemRows != 1 {
		t.Fatalf("system event rows=%d err=%v, want 1", systemRows, err)
	}
	for _, c := range comments {
		if _, ok := ytcOpFor(t, s, c.ID); !ok {
			t.Errorf("comment %d has no comments-kind operation", c.ID)
		}
	}
}

func TestNotAppliedTaskEventDoesNotCreateComment(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateCompleted, false)
	res := send(t, s, mergeEvent(task.ID, testPR(4733)))
	if res.Applied || len(sourceRows(t, s, task.ID, SourceMergewakeMerge)) != 1 {
		t.Fatalf("expected recorded but unapplied merge event: %+v", res)
	}
	if got := eventComments(t, s, task.ID); len(got) != 0 {
		t.Fatalf("unapplied event produced comments: %+v", got)
	}
}

func TestRevertedPRAdoptionDoesNotCreateComment(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateMerged, false)
	child := flowTask(t, s, pool, "implement", StateDeveloping, true)
	setParent(t, s, child, parent)
	pr, revertPR := testPR(4735), testPR(5735)
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = insertEvent(t.Context(), pgTransaction{tx}, taskEventInput{
		TaskID: parent.ID, Kind: TaskEventSource, From: StateMerged, To: StateMerged, By: SourceMergewakeRevert,
		At: time.Now().UTC(), Source: SourceMergewakeRevert, SourceRef: revertPR,
		Field: revertedPRField, ToValue: pr, Auto: true, NotApplied: true,
	})
	if err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := send(t, s, mergeEvent(parent.ID, pr))
	if res.Applied || res.Reason == "" {
		t.Fatalf("reverted PR adoption unexpectedly applied: %+v", res)
	}
	if got := eventComments(t, s, parent.ID); len(got) != 0 {
		t.Fatalf("refused PR adoption produced comments: %+v", got)
	}
}

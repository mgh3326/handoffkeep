package store

import (
	"strings"
	"testing"
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

func TestTaskEventCommentRefusalDoesNotFailEvent(t *testing.T) {
	s, pool := searchTestStore(t)
	task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	ev := testEvent(SourceMergewakePR, 4732)
	ev.CommentBody = "please include AKIAABCDEFGHIJKLMNOP"
	res := applyEvent(t, s, task.ID, ev)
	if !res.Applied || len(sourceRows(t, s, task.ID, SourceMergewakePR)) != 1 {
		t.Fatalf("event failed with comment refusal: %+v", res)
	}
	if got := eventComments(t, s, task.ID); len(got) != 0 {
		t.Fatalf("secret-like comment was stored: %+v", got)
	}
}

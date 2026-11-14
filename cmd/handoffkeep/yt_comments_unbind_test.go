package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack comments-unbind` and `--task N` on the comments preflight and
// backfill (hk 4490).

func TestYTUsageNamesCommentsUnbind(t *testing.T) {
	err := run([]string{"yt", "nonsense"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "comments-unbind") || !strings.Contains(err.Error(), "--task N") {
		t.Fatalf("yt usage: %v", err)
	}
	for _, args := range [][]string{
		{"yt", "comments-unbind"},
		{"yt", "comments-unbind", "x"},
		{"yt", "comments-unbind", "0"},
		{"yt", "comments-unbind", "1", "2"},
		{"yt", "comments-unbind", "1", "--by", " "},
		{"yt", "comments-unbind", "1", "--nonsense"},
		{"yt", "comments-preflight", "--task", "-1"},
		{"yt", "comments-preflight", "--task", "1", "--project", "p"},
		{"yt", "comments-backfill", "--task", "1", "--all"},
		{"yt", "comments-backfill", "--task", "0"},
		{"yt", "comments-backfill", "--task", "0", "--all"},
		{"yt", "comments-preflight", "--task", "0"},
	} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	var code exitCodeError
	if err := run([]string{"yt", "comments-unbind", "1"}, io.Discard, io.Discard); !errors.As(err, &code) || code.code != youtrackExitConfig {
		t.Errorf("no database: %v, want exit %d", err, youtrackExitConfig)
	}
}

type ytuResult struct {
	TaskID   int64                `json:"task_id"`
	Removed  bool                 `json:"removed"`
	Binding  *store.YTTaskBinding `json:"binding"`
	Rejected []struct {
		OpID   int64  `json:"op_id"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"rejected"`
	Released       []int64 `json:"released"`
	Stranded       []int64 `json:"stranded"`
	AnchorReleased bool    `json:"anchor_released"`
}

// An operator unbinds a task, the rejected op is reported, and
// `comments-backfill --task N` binds exactly that task again, to the issue it
// verifies: also an issue other than the one the task's first comment
// anchored in yt_entity_map, which the unbind released (hk 4536).
func TestCommentsUnbindThenRebindOneTask(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytu_cmd")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	fake := &ytpFake{issues: map[string]map[string]any{}, fail: map[string]int{}, hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host
	const project = "ytu"
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	newTask := func() int64 {
		t.Helper()
		p := project
		x, err := st.CreateTask(ctx, store.Task{Lane: "ytu-lane", Title: "unbind", Kind: "implement", CreatedBy: "test", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	bind := func(task int64) {
		t.Helper()
		verified, err := st.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		id := strconv.FormatInt(task, 10)
		if _, err := st.AddYTTaskBinding(ctx, store.YTTaskBinding{TaskID: task, Tenant: tenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", BoundBy: "test"}, verified); err != nil {
			t.Fatal(err)
		}
	}
	target, bystander := newTask(), newTask()
	for _, task := range []int64{target, bystander} {
		fake.add(task)
	}
	bind(target)
	st.SetYTCommentsMode(store.YTCommentsOn)
	c, err := st.CreateTaskCommentProjected(ctx, target, "worker", "prepared before the unbind")
	if err != nil || c.YouTrackProjection == nil {
		t.Fatalf("comment: %+v %v", c.YouTrackProjection, err)
	}
	st.SetYTCommentsMode(store.YTCommentsOff)

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")
	runJSON := func(into any, args ...string) error {
		t.Helper()
		var out bytes.Buffer
		err := run(args, &out, io.Discard)
		if out.Len() > 0 {
			if jerr := json.Unmarshal(out.Bytes(), into); jerr != nil {
				t.Fatalf("%v: output %q: %v", args, out.String(), jerr)
			}
		}
		return err
	}

	var res ytuResult
	id := strconv.FormatInt(target, 10)
	if err := runJSON(&res, "yt", "comments-unbind", id, "--reason", "wrong issue", "--by", "operator:test"); err != nil {
		t.Fatal(err)
	}
	if res.TaskID != target || !res.Removed || res.Binding == nil || res.Binding.IssueID != "2-"+id || len(res.Rejected) != 1 ||
		res.Rejected[0].Kind != ytwrite.KindCommentAppend || res.Rejected[0].Reason != ytwrite.ReasonBindingRemoved ||
		len(res.Released) != 1 || res.Released[0] != c.ID || len(res.Stranded) != 0 || !res.AnchorReleased {
		t.Fatalf("unbind result %+v", res)
	}
	if _, found, err := st.YTTaskBinding(ctx, target); err != nil || found {
		t.Fatalf("binding after unbind: %v %v", found, err)
	}
	if cand, err := st.YTCanaryCandidate(ctx, target); err != nil || cand.MappedIssueID != "" {
		t.Fatalf("candidate after unbind: %+v %v, want no mapped issue", cand, err)
	}
	// Idempotent: a second run reports nothing removed.
	res = ytuResult{}
	if err := runJSON(&res, "yt", "comments-unbind", id); err != nil || res.Removed || res.Binding != nil || res.AnchorReleased {
		t.Fatalf("second unbind: %+v %v", res, err)
	}

	// The operator's issue is another one now (recreated under a new id).
	fake.mu.Lock()
	fake.issues[ytpIssuePath(target)]["id"] = "2-recreated"
	fake.mu.Unlock()

	// --task N covers that task alone, whatever else would verify.
	var report ytpReport
	var raw struct {
		Task int64 `json:"task"`
	}
	var out bytes.Buffer
	if err := run([]string{"yt", "comments-backfill", "--task", id}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil || raw.Task != target {
		t.Fatalf("report task %d (%v), want %d", raw.Task, err, target)
	}
	if report.BoundNow == nil || *report.BoundNow != 1 {
		t.Fatalf("bound_now %v, want 1", report.BoundNow)
	}
	p := report.project(t, project)
	ytpIDs(t, "bound", p.Bound)
	ytpIDs(t, "verifies", p.Verifies, target)
	if b, found, err := st.YTTaskBinding(ctx, target); err != nil || !found || b.Origin != store.YTBindingImportVerified || b.IssueID != "2-recreated" {
		t.Fatalf("rebind: %+v %v %v, want import_verified on 2-recreated", b, found, err)
	}
	if _, found, _ := st.YTTaskBinding(ctx, bystander); found {
		t.Fatal("--task bound another task")
	}
	// A task that does not exist is an error, not an empty report.
	if err := run([]string{"yt", "comments-preflight", "--task", "987654321"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing task: %v", err)
	}
}

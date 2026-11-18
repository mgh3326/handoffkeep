package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The waits commands (hk 5128) send the request the store expects: --on splits
// at the first colon, an operator wait has no ref, and an unwait names its task
// and wait ids.
func TestTasksWaitCommands(t *testing.T) {
	server, last := fakeHK(t, store.TaskWait{ID: 7, TaskID: 9, OnKind: "lane", OnRef: "handoffkeep-design", Reason: "review"})
	var out, errout bytes.Buffer
	base := []string{"--url", server.URL, "--token", "t"}

	if err := run(append([]string{"tasks", "wait", "9", "--on", "lane:handoffkeep-design", "--for", "review"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Method != "POST" || last.Path != "/v1/tasks/9/waits" || !strings.Contains(last.Body, `"on_kind":"lane"`) || !strings.Contains(last.Body, `"on_ref":"handoffkeep-design"`) || !strings.Contains(last.Body, `"reason":"review"`) {
		t.Fatalf("lane wait request: %+v", *last)
	}

	if err := run(append([]string{"tasks", "wait", "9", "--on", "operator", "--for", "register the device key"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Body, `"on_kind":"operator"`) || !strings.Contains(last.Body, `"on_ref":""`) {
		t.Fatalf("operator wait request: %+v", *last)
	}

	if err := run(append([]string{"tasks", "wait", "9", "--on", "external:spawn", "--for", "502 at hub-accept"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Body, `"on_kind":"external"`) || !strings.Contains(last.Body, `"on_ref":"spawn"`) {
		t.Fatalf("spawn failure request: %+v", *last)
	}

	if err := run(append([]string{"tasks", "unwait", "9", "7", "--note", "done"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Method != "POST" || last.Path != "/v1/tasks/9/waits/7/clear" || !strings.Contains(last.Body, `"note":"done"`) {
		t.Fatalf("unwait request: %+v", *last)
	}

	for _, bad := range [][]string{
		{"tasks", "wait", "9", "--for", "review"},
		{"tasks", "wait", "9", "--on", "lane:x"},
		{"tasks", "wait", "0", "--on", "operator", "--for", "x"},
		{"tasks", "unwait", "9"},
		{"tasks", "unwait", "9", "-1"},
	} {
		if err := run(append(bad, base...), &out, &errout); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func TestWaitsListCommandSendsSection(t *testing.T) {
	server, last := fakeHK(t, store.WaitsView{})
	var out, errout bytes.Buffer
	base := []string{"--url", server.URL, "--token", "t"}
	if err := run(append([]string{"waits", "list", "--section", "failed_start"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Method != "GET" || !strings.HasPrefix(last.Path, "/v1/waits") {
		t.Fatalf("waits list request: %+v", *last)
	}
	if err := run(append([]string{"waits", "list", "extra"}, base...), &out, &errout); err == nil {
		t.Fatal("a positional argument to waits list must be refused")
	}
}

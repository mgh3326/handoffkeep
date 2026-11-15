package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `youtrack links status` (hk 4640). The output is the server's body printed
// as JSON, unchanged field by field: the golden string below is that body.
const youtrackLinksGolden = `{"mode":"on","tenant":"yt.example","open":true,"task_id":7,"limit":100,` +
	`"sweep":{"generation":3,"done_generation":3,"after_task_id":0,"complete":true},` +
	`"owned_open":1,"owned":[{"id":11,"link_type":"relates","edge":"related","from_issue":"2-7","to_issue":"2-9",` +
	`"writer_task_id":7,"target_task_id":9,"intended_op_id":41,"intended_at":"2026-11-14T10:00:00Z"}],` +
	`"unowned_open":0,"unowned":[],` +
	`"held_count":1,"held":[{"op_id":42,"task_id":7,"yt_target_id":"2-9","state":"prepared","reason":"target_unbound","created_at":"2026-11-14T10:05:00Z"}]}` + "\n"

func linksStatusServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/yt/links/status" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestYTLinksStatusPrintsTheServerBodyForOneTask(t *testing.T) {
	server := linksStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("task"); got != "7" {
			t.Errorf("task query = %q, want 7", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, youtrackLinksGolden)
	})
	var out strings.Builder
	if err := run([]string{"youtrack", "links", "status", "--task", "7", "--url", server.URL, "--token", "t"}, &out, io.Discard); err != nil {
		t.Fatalf("links status --task 7: %v", err)
	}
	if out.String() != youtrackLinksGolden {
		t.Fatalf("output\n%s\nwant\n%s", out.String(), youtrackLinksGolden)
	}
}

// Without --task the query names no task and asks for the default page size.
func TestYTLinksStatusAllTasksQuery(t *testing.T) {
	server := linksStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("task") {
			t.Errorf("all-tasks query carries task=%q", r.URL.Query().Get("task"))
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit query = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mode":"off","tenant":"","open":false,"task_id":0,"limit":100,`+
			`"sweep":{"generation":0,"done_generation":0,"after_task_id":0,"complete":true},`+
			`"owned_open":0,"owned":[],"unowned_open":0,"unowned":[],"held_count":0,"held":[]}`)
	})
	var out strings.Builder
	if err := run([]string{"youtrack", "links", "status", "--url", server.URL, "--token", "t"}, &out, io.Discard); err != nil {
		t.Fatalf("links status: %v", err)
	}
	if !strings.Contains(out.String(), `"mode":"off"`) || !strings.Contains(out.String(), `"held":[]`) {
		t.Fatalf("all-tasks output: %s", out.String())
	}
}

// An unknown task exits with the not_found code, not the generic failure.
func TestYTLinksStatusUnknownTaskExitsNotFound(t *testing.T) {
	server := linksStatusServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	})
	err := run([]string{"youtrack", "links", "status", "--task", "99", "--url", server.URL, "--token", "t"}, io.Discard, io.Discard)
	var exit exitCodeError
	if !errors.As(err, &exit) || exit.code != youtrackExitNotFound {
		t.Fatalf("unknown task: err=%v, want exit code %d", err, youtrackExitNotFound)
	}
}

func TestYTLinksStatusRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"youtrack", "links"},
		{"youtrack", "links", "adopt"},
		{"youtrack", "links", "status", "extra"},
		{"youtrack", "links", "status", "--task=-1"},
		{"youtrack", "links", "status", "--limit", "0"},
	} {
		if err := run(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "links status") {
			t.Errorf("%v: err=%v, want the usage", args, err)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeRequest struct{ Method, Path, Query, Body string }

// recordingHK answers every request with response and records them all.
func recordingHK(t *testing.T, response any) (*httptest.Server, *[]fakeRequest) {
	t.Helper()
	var seen []fakeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		seen = append(seen, fakeRequest{r.Method, r.URL.Path, r.URL.RawQuery, b.String()})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func TestTaskOriginCmd(t *testing.T) {
	const uuid = "3f2b8c1e-9a4d-4e6f-8b7a-1c2d3e4f5a6b"
	origin := store.TaskOrigin{ID: 4, TaskID: 9, Provider: "linear", Namespace: "example-ws", ExternalID: uuid, DisplayRef: "ROB-1514"}
	server, seen := recordingHK(t, map[string]any{"origin": origin, "created": true, "origins": []store.TaskOrigin{origin}})
	base := []string{"--url", server.URL, "--token", "t"}

	var out, errout bytes.Buffer
	args := append([]string{"tasks", "origin", "add", "9", "--provider", "linear", "--namespace", "example-ws", "--external-id", uuid, "--display-ref", "ROB-1514", "--origin-url", "https://linear.app/example-ws/issue/ROB-1514", "--title", "an issue"}, base...)
	if err := run(args, &out, &errout); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Method != "POST" || (*seen)[0].Path != "/v1/tasks/9/origins" {
		t.Fatalf("add request: %+v", *seen)
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte((*seen)[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"provider": "linear", "namespace": "example-ws", "external_id": uuid, "display_ref": "ROB-1514", "url": "https://linear.app/example-ws/issue/ROB-1514", "title": "an issue"} {
		if sent[k] != want {
			t.Errorf("body %s=%q, want %q (body %s)", k, sent[k], want, (*seen)[0].Body)
		}
	}
	var printed struct {
		Origin  store.TaskOrigin `json:"origin"`
		Created bool             `json:"created"`
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil || !printed.Created || printed.Origin.DisplayRef != "ROB-1514" {
		t.Fatalf("add output %q err=%v", out.String(), err)
	}

	// Flags and the id may come in any order.
	out.Reset()
	*seen = nil
	args = append([]string{"tasks", "origin", "add", "--provider", "github", "--namespace", "mgh3326/handoffkeep", "--external-id", "I_kwDOabc", "9"}, base...)
	if err := run(args, &out, &errout); err != nil || (*seen)[0].Path != "/v1/tasks/9/origins" {
		t.Fatalf("flags before id: %v %+v", err, *seen)
	}

	out.Reset()
	*seen = nil
	if err := run(append([]string{"tasks", "origin", "list", "9"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v1/tasks/9/origins" {
		t.Fatalf("list request: %+v", *seen)
	}
	var listed struct {
		Origins []store.TaskOrigin `json:"origins"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Origins) != 1 {
		t.Fatalf("list output %q err=%v", out.String(), err)
	}

	out.Reset()
	*seen = nil
	if err := run(append([]string{"tasks", "origin", "find", "--provider", "linear", "--external-id", uuid}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v1/task-origins" || (*seen)[0].Query != "external_id="+uuid+"&provider=linear" {
		t.Fatalf("find request: %+v", *seen)
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Origins) != 1 || listed.Origins[0].TaskID != 9 {
		t.Fatalf("find output %q err=%v", out.String(), err)
	}

	// A missing required flag or a bad id is refused before any request.
	*seen = nil
	out.Reset()
	for name, bad := range map[string][]string{
		"add without --external-id":  {"tasks", "origin", "add", "9", "--provider", "linear", "--namespace", "example-ws"},
		"add without --provider":     {"tasks", "origin", "add", "9", "--namespace", "example-ws", "--external-id", uuid},
		"add without --namespace":    {"tasks", "origin", "add", "9", "--provider", "linear", "--external-id", uuid},
		"add without an id":          {"tasks", "origin", "add", "--provider", "linear", "--namespace", "example-ws", "--external-id", uuid},
		"add with id 0":              {"tasks", "origin", "add", "0", "--provider", "linear", "--namespace", "example-ws", "--external-id", uuid},
		"list without an id":         {"tasks", "origin", "list"},
		"find without --external-id": {"tasks", "origin", "find", "--provider", "linear"},
		"find without --provider":    {"tasks", "origin", "find", "--external-id", uuid},
		"find with an id":            {"tasks", "origin", "find", "9", "--provider", "linear", "--external-id", uuid},
		"no subcommand":              {"tasks", "origin"},
		"unknown subcommand":         {"tasks", "origin", "remove", "9"},
		"list with --external-id":    {"tasks", "origin", "list", "9", "--external-id", uuid},
	} {
		if err := run(append(bad, base...), &out, &errout); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*seen) != 0 || out.Len() != 0 {
		t.Fatalf("a refused command sent %+v and printed %q", *seen, out.String())
	}
}

func TestDocPutTaskAndOriginClass(t *testing.T) {
	server, seen := recordingHK(t, map[string]any{"document": store.Document{Key: "k", Kind: "design"}, "changed": true})
	base := []string{"--url", server.URL, "--token", "t"}
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	args := append([]string{"doc", "put", "--key", "design/x", "--kind", "design", "--file", file, "--task", "12", "--origin-class", "job"}, base...)
	if err := run(args, &out, &errout); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*seen)[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Method != "PUT" || (*seen)[0].Path != "/v1/documents/design/x" || sent["task_id"] != float64(12) || sent["origin_class"] != "job" || sent["kind"] != "design" {
		t.Fatalf("put request: %+v", *seen)
	}

	// An old-shape put sends neither field.
	*seen = nil
	if err := run(append([]string{"doc", "put", "--key", "n/x", "--file", file}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*seen)[0].Body, "task_id") || strings.Contains((*seen)[0].Body, "origin_class") {
		t.Fatalf("old-shape put body: %s", (*seen)[0].Body)
	}

	*seen = nil
	for name, bad := range map[string][]string{
		"task 0":               {"doc", "put", "--key", "k", "--file", file, "--task", "0"},
		"task -2":              {"doc", "put", "--key", "k", "--file", file, "--task=-2"},
		"task on get":          {"doc", "get", "k", "--task", "3"},
		"origin-class on list": {"doc", "list", "--origin-class", "job"},
	} {
		if err := run(append(bad, base...), &out, &errout); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("a refused command sent %+v", *seen)
	}
}

func TestDocListTask(t *testing.T) {
	task := int64(12)
	other := int64(13)
	server, seen := recordingHK(t, map[string]any{"documents": []store.Document{{Key: "a", TaskID: &task}, {Key: "b", TaskID: &other}, {Key: "c"}}})
	base := []string{"--url", server.URL, "--token", "t"}
	var out, errout bytes.Buffer
	if err := run(append([]string{"doc", "list", "--task", "12", "--prefix", "p/"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Path != "/v1/documents" || !strings.Contains((*seen)[0].Query, "task=12") || !strings.Contains((*seen)[0].Query, "prefix=p%2F") {
		t.Fatalf("list request: %+v", *seen)
	}
	// A server that predates the filter answers with every document; only the
	// task's own are printed.
	var docs []store.Document
	if err := json.Unmarshal(out.Bytes(), &docs); err != nil || len(docs) != 1 || docs[0].Key != "a" {
		t.Fatalf("list output %q err=%v", out.String(), err)
	}
	// Without --task the request carries no task parameter.
	*seen = nil
	out.Reset()
	if err := run(append([]string{"doc", "list"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*seen)[0].Query, "task") {
		t.Fatalf("list without --task sent %q", (*seen)[0].Query)
	}
	if err := json.Unmarshal(out.Bytes(), &docs); err != nil || len(docs) != 3 {
		t.Fatalf("unfiltered output %q err=%v", out.String(), err)
	}
}

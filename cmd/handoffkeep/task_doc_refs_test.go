package main

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestTaskDocRefCLI checks the wire calls behind "tasks doc-ref": one POST per
// --doc on add, a DELETE that names the document in the query on rm, and a
// GET on list. Missing or misplaced --doc flags are refused before any call.
func TestTaskDocRefCLI(t *testing.T) {
	var calls []orderCall
	h := orderServer(t, &calls, func(string) (int, any) {
		return http.StatusOK, map[string]any{"changed": true, "doc_refs": []any{}}
	})
	base := []string{"--url", h.URL, "--token", "tok"}

	if err := run(append([]string{"tasks", "doc-ref", "add", "42", "--doc", "tbls/a/doc", "--doc", "tbls/b/doc"}, base...), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].method != "POST" || calls[0].path != "/v1/tasks/42/doc-refs" || calls[0].body["doc"] != "tbls/a/doc" || calls[1].body["doc"] != "tbls/b/doc" {
		t.Fatalf("add calls = %+v, want two POSTs naming each doc", calls)
	}

	calls = nil
	if err := run(append([]string{"tasks", "doc-ref", "rm", "42", "--doc", "tbls/a/doc"}, base...), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].method != "DELETE" || !strings.HasPrefix(calls[0].path, "/v1/tasks/42/doc-refs?doc=") {
		t.Fatalf("rm calls = %+v, want one DELETE with the doc query", calls)
	}

	calls = nil
	if err := run(append([]string{"tasks", "doc-ref", "list", "42"}, base...), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].method != "GET" || calls[0].path != "/v1/tasks/42/doc-refs" {
		t.Fatalf("list calls = %+v, want one GET", calls)
	}

	for _, bad := range [][]string{
		{"tasks", "doc-ref", "add", "42"},
		{"tasks", "doc-ref", "rm", "42"},
		{"tasks", "doc-ref", "list", "42", "--doc", "tbls/a/doc"},
		{"tasks", "doc-ref", "add", "0", "--doc", "tbls/a/doc"},
		{"tasks", "doc-ref", "bogus", "42"},
	} {
		calls = nil
		if err := run(append(bad, base...), io.Discard, io.Discard); err == nil {
			t.Fatalf("%v: want refusal", bad)
		}
		if len(calls) != 0 {
			t.Fatalf("%v: refused call still sent %+v", bad, calls)
		}
	}
}

// TestDotsRequestDocsFlag checks that each --doc on dots request is sent as the
// request's docs list, in the order given.
func TestDotsRequestDocsFlag(t *testing.T) {
	var calls []orderCall
	h := orderServer(t, &calls, func(string) (int, any) {
		return http.StatusCreated, map[string]any{"task": map[string]any{"id": 7}}
	})
	args := []string{"dots", "request", "--title", "verify schema", "--body", "check it", "--requester-lane", "lane-x", "--project", "p",
		"--doc", "tbls/a/doc", "--doc", "tbls/b/schema.json", "--url", h.URL, "--token", "tok"}
	if err := run(args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].path != "/v1/dots/requests" {
		t.Fatalf("calls = %+v, want one POST to /v1/dots/requests", calls)
	}
	if got := calls[0].body["docs"]; !reflect.DeepEqual(got, []any{"tbls/a/doc", "tbls/b/schema.json"}) {
		t.Fatalf("docs = %#v, want both keys in order", got)
	}
}

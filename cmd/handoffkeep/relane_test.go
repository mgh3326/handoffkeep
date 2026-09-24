package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type relaneRequest struct {
	Method, Path, Auth string
	Body               map[string]any
}

// relaneServer answers every POST /v1/tasks/relane with a canned body and
// records what arrived.
func relaneServer(t *testing.T, status int, response string) (*httptest.Server, *[]relaneRequest) {
	t.Helper()
	var seen []relaneRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := relaneRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

const relaneOKBody = `{"results":[{"id":42,"ok":true,"changed":true,"task":{"id":42,"lane":"director-1"}}],"moved":1,"unchanged":0,"failed":0}`

// Single-id form: positional id, flags anywhere.
func TestTasksRelaneCLISingleID(t *testing.T) {
	h, seen := relaneServer(t, 200, relaneOKBody)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"tasks", "relane", "42", "--to", "director-1", "--note", "triage", "--url", h.URL, "--token", "tok"},
		{"tasks", "relane", "--to", "director-1", "--note", "triage", "42", "--url", h.URL, "--token", "tok"},
	} {
		out.Reset()
		if err := run(args, &out, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), `"changed":true`) {
			t.Fatalf("out=%s", out.String())
		}
	}
	if len(*seen) != 2 {
		t.Fatalf("requests=%+v", *seen)
	}
	req := (*seen)[0]
	if req.Method != "POST" || req.Path != "/v1/tasks/relane" || req.Auth != "Bearer tok" {
		t.Fatalf("request=%+v", req)
	}
	if ids, ok := req.Body["ids"].([]any); !ok || len(ids) != 1 || ids[0] != float64(42) {
		t.Fatalf("ids=%v", req.Body["ids"])
	}
	if req.Body["to"] != "director-1" || req.Body["note"] != "triage" {
		t.Fatalf("body=%v", req.Body)
	}
	if _, present := req.Body["by"]; present {
		t.Fatalf("client must not send by: %v", req.Body)
	}
	if _, present := req.Body["allow_new_lane"]; present {
		t.Fatalf("allow_new_lane must be omitted when unset: %v", req.Body)
	}
}

// --ids takes a comma list (repeatable) and "-" reads whitespace-separated
// ids from stdin.
func TestTasksRelaneCLIBatchIDs(t *testing.T) {
	body := `{"results":[{"id":33,"ok":true,"changed":true},{"id":37,"ok":true,"changed":true},{"id":38,"ok":true,"changed":true}],"moved":3,"unchanged":0,"failed":0}`
	h, seen := relaneServer(t, 200, body)
	var out bytes.Buffer
	if err := run([]string{"tasks", "relane", "--ids", "33,37", "--ids", "38", "--to", "director-1", "--note", "n", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if ids := (*seen)[0].Body["ids"].([]any); len(ids) != 3 || ids[0] != float64(33) || ids[1] != float64(37) || ids[2] != float64(38) {
		t.Fatalf("ids=%v", (*seen)[0].Body["ids"])
	}
	out.Reset()
	// Positional id and --ids combine; stdin "-" expands whitespace-separated.
	err := taskRelaneCmd([]string{"relane", "33", "--ids", "-", "--to", "director-1", "--note", "n", "--url", h.URL, "--token", "tok"}, strings.NewReader("37 38\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ids := (*seen)[1].Body["ids"].([]any); len(ids) != 3 {
		t.Fatalf("stdin ids=%v", (*seen)[1].Body["ids"])
	}
	// --allow-new-lane passes through only when set.
	h2, seen2 := relaneServer(t, 200, relaneOKBody)
	out.Reset()
	if err := run([]string{"tasks", "relane", "42", "--to", "new-lane", "--note", "n", "--allow-new-lane", "--url", h2.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if (*seen2)[0].Body["allow_new_lane"] != true {
		t.Fatalf("allow_new_lane=%v", (*seen2)[0].Body["allow_new_lane"])
	}
}

// Any failed item exits nonzero, but the per-item results still print.
func TestTasksRelaneCLIPartialFailureExitsNonzero(t *testing.T) {
	body := `{"results":[{"id":33,"ok":true,"changed":true},{"id":999,"ok":false,"error":"not_found"}],"moved":1,"unchanged":0,"failed":1}`
	h, _ := relaneServer(t, 200, body)
	var out bytes.Buffer
	err := run([]string{"tasks", "relane", "--ids", "33,999", "--to", "director-1", "--note", "n", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "relane failed for 1 of 2") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(out.String(), `"error":"not_found"`) {
		t.Fatalf("results lost on failure: out=%s", out.String())
	}
}

// A pre-relane server has no route: the CLI surfaces a plain http_404 rather
// than a blank error.
func TestTasksRelaneCLIOldServer(t *testing.T) {
	h, _ := relaneServer(t, 404, `404 page not found`)
	var out bytes.Buffer
	err := run([]string{"tasks", "relane", "999999", "--to", "director-1", "--note", "probe", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "http_404") {
		t.Fatalf("err=%v, want http_404", err)
	}
}

func TestTasksRelaneCLIRejectsBadInputBeforeSending(t *testing.T) {
	h, seen := relaneServer(t, 200, relaneOKBody)
	base := []string{"--url", h.URL, "--token", "tok"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"tasks", "relane", "--to", "x", "--note", "n"}, "requires an id or --ids"},
		{[]string{"tasks", "relane", "42", "--note", "n"}, "requires --to"},
		{[]string{"tasks", "relane", "42", "--to", "x"}, "requires --note"},
		{[]string{"tasks", "relane", "42", "--to", "x", "--note", "  "}, "requires --note"},
		{[]string{"tasks", "relane", "abc", "--to", "x", "--note", "n"}, "must be positive"},
		{[]string{"tasks", "relane", "0", "--to", "x", "--note", "n"}, "must be positive"},
		{[]string{"tasks", "relane", "--ids", "a,b", "--to", "x", "--note", "n"}, "must be positive"},
		{[]string{"tasks", "relane", "1", "2", "--to", "x", "--note", "n"}, "usage: tasks relane"},
	} {
		err := run(append(tc.args, base...), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err=%v want %q", tc.args, err, tc.want)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("invalid input reached the server: %+v", *seen)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type intakeRequest struct {
	Method, Path string
	Body         map[string]json.RawMessage
}

// intakeServer answers every request with status and reply and records it.
func intakeServer(t *testing.T, status int, reply string) (*httptest.Server, *[]intakeRequest) {
	t.Helper()
	var seen []intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

func runAgainst(h *httptest.Server, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(append(args, "--url", h.URL, "--token", "tok"), &out, io.Discard)
	return out.String(), err
}

func TestTasksAddDuplicateFlags(t *testing.T) {
	h, seen := intakeServer(t, http.StatusCreated, `{"id":9,"project":"experiment"}`)
	add := []string{"tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment"}
	if _, err := runAgainst(h, append(add, "--link", "4")...); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgainst(h, append(add, "--new")...); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgainst(h, add...); err != nil {
		t.Fatal(err)
	}
	if got := string((*seen)[0].Body["link"]); got != "4" {
		t.Fatalf("--link sent %q", got)
	}
	if got := string((*seen)[1].Body["new"]); got != "true" {
		t.Fatalf("--new sent %q", got)
	}
	if _, ok := (*seen)[2].Body["link"]; ok {
		t.Fatalf("plain add sent link: %v", (*seen)[2].Body)
	}
	n := len(*seen)
	for name, args := range map[string][]string{
		"both":         append(append([]string{}, add...), "--link", "4", "--new"),
		"zero link":    append(append([]string{}, add...), "--link", "0"),
		"link on list": {"tasks", "list", "--link", "4"},
		"new on list":  {"tasks", "list", "--new"},
		"set on add":   append(append([]string{}, add...), "--set"),
	} {
		if _, err := runAgainst(h, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*seen) != n {
		t.Fatalf("refused flags reached the server: %+v", (*seen)[n:])
	}
}

// A 409 similar_tasks names the candidates and the two ways forward.
func TestTasksAddSimilarTasksMessage(t *testing.T) {
	h, _ := intakeServer(t, http.StatusConflict, `{"error":"similar_tasks","similar_tasks":[{"id":4,"title":"x again","state":"backlog","lane":"d","similarity":0.9}]}`)
	_, err := runAgainst(h, "tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment")
	if err == nil || !strings.Contains(err.Error(), `#4 "x again" (backlog)`) || !strings.Contains(err.Error(), "--link <id>") || !strings.Contains(err.Error(), "--new") {
		t.Fatalf("err=%v", err)
	}
}

func TestTasksIntakeCommand(t *testing.T) {
	h, seen := intakeServer(t, http.StatusCreated, `{"created":true,"task":{"id":5,"state":"triage"}}`)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("from a file"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runAgainst(h, "tasks", "intake", "--source", "dots", "--source-ref", "c-1", "--title", "follow up", "--body-file", bodyFile, "--project", "experiment", "--priority", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"created":true`) {
		t.Fatalf("out=%s", out)
	}
	req := (*seen)[0]
	if req.Path != "/v1/intake" || string(req.Body["body"]) != `"from a file"` || string(req.Body["project"]) != `"experiment"` || string(req.Body["priority"]) != "2" {
		t.Fatalf("request=%+v", req)
	}
	if _, ok := req.Body["kind"]; ok {
		t.Fatalf("unset kind sent: %v", req.Body)
	}
	for name, args := range map[string][]string{
		"no ref":     {"tasks", "intake", "--source", "dots", "--title", "t"},
		"two bodies": {"tasks", "intake", "--source", "dots", "--source-ref", "r", "--title", "t", "--body", "b", "--body-file", bodyFile},
	} {
		if _, err := runAgainst(h, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("refused intake reached the server: %+v", *seen)
	}
}

func TestTicketsEmitCommand(t *testing.T) {
	h, seen := intakeServer(t, http.StatusCreated, `{"id":1,"source":"repo-session","source_ref":"s","parent_id":10,"children":[{"ref":"a","id":11},{"ref":"b","id":12}],"created":true}`)
	doc := `{"source":"repo-session","source_ref":"s","parent":{"title":"p","body":"b"},"children":[{"ref":"a","title":"a","body":"b"},{"ref":"b","title":"b","body":"b","blocked_by":["a",3]}]}`
	file := filepath.Join(t.TempDir(), "set.json")
	if err := os.WriteFile(file, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runAgainst(h, "tickets", "emit", "--file", file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"parent_id":10`) || !strings.Contains(out, `"created":true`) {
		t.Fatalf("out=%s", out)
	}
	req := (*seen)[0]
	if req.Method != http.MethodPost || req.Path != "/v1/ticket-sets" || !strings.Contains(string(req.Body["children"]), `"blocked_by":["a",3]`) {
		t.Fatalf("request=%+v children=%s", req, req.Body["children"])
	}
	// A key the server does not know fails before any request.
	typo := filepath.Join(t.TempDir(), "typo.json")
	if err := os.WriteFile(typo, []byte(`{"source":"repo-session","source_ref":"s","parnet":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgainst(h, "tickets", "emit", "--file", typo); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := runAgainst(h, "tickets", "emit"); err == nil {
		t.Fatal("missing --file accepted")
	}
	if len(*seen) != 1 {
		t.Fatalf("refused emits reached the server: %+v", (*seen)[1:])
	}
}

// tasks transition sends the triage fields and --set only when given.
func TestTasksTransitionTriageFlags(t *testing.T) {
	h, seen := intakeServer(t, http.StatusOK, `{"id":3,"state":"backlog"}`)
	if _, err := runAgainst(h, "tasks", "transition", "3", "--to", "backlog", "--priority", "2", "--kind", "fix", "--project", "experiment", "--set"); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgainst(h, "tasks", "transition", "3", "--to", "backlog"); err != nil {
		t.Fatal(err)
	}
	with, without := (*seen)[0].Body, (*seen)[1].Body
	if string(with["priority"]) != "2" || string(with["kind"]) != `"fix"` || string(with["project"]) != `"experiment"` || string(with["set"]) != "true" {
		t.Fatalf("with=%v", with)
	}
	for _, k := range []string{"priority", "kind", "project", "set"} {
		if _, ok := without[k]; ok {
			t.Fatalf("unset %s sent: %v", k, without)
		}
	}
}

func TestTasksTriageCommand(t *testing.T) {
	h, seen := intakeServer(t, http.StatusOK, `{"triage":[{"task":{"id":3,"title":"p"},"ticket_set":true,"child_count":2,"children_in_triage":2,"age_seconds":5,"similar":[]}]}`)
	out, err := runAgainst(h, "tasks", "triage", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Path != "/v1/triage?limit=10" || !strings.Contains(out, `"child_count":2`) {
		t.Fatalf("path=%s out=%s", (*seen)[0].Path, out)
	}
	if _, err := runAgainst(h, "tasks", "triage", "--limit", "0"); err == nil {
		t.Fatal("limit 0 accepted")
	}
}

func TestTasksTransitionClosureFlags(t *testing.T) {
	h, seen := intakeServer(t, http.StatusOK, `{"id":7,"state":"completed"}`)
	if _, err := runAgainst(h, "tasks", "transition", "7", "--to", "completed", "--evidence", "https://x/r"); err != nil {
		t.Fatal(err)
	}
	body := (*seen)[0].Body
	if len(body) != 2 || string(body["to"]) != `"completed"` || string(body["evidence"]) != `"https://x/r"` {
		t.Fatalf("body=%v", body)
	}
	if _, err := runAgainst(h, "tasks", "add", "--evidence", "x"); err == nil || !strings.Contains(err.Error(), "--closure/--evidence are accepted only by tasks transition") {
		t.Fatalf("err=%v", err)
	}
}

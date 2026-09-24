package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const decisionRequestOKBody = `{"task":{"id":618,"state":"in_progress"},"request":{"id":"dr-618-1","revision":1,"status":"open","question":"q","default_action":"보류하고 다음 태스크","requested_by":"director-1","requested_at":"2026-09-24T01:00:00Z"},"duplicate":false}`

// A6: the request is recorded first and the output hands back the
// request_id for the pane message.
func TestTasksDecisionRequestCLIRecordsThenPrintsRequestID(t *testing.T) {
	h, seen := relaneServer(t, 201, decisionRequestOKBody)
	var out bytes.Buffer
	args := []string{"tasks", "decision-request", "618",
		"--question", "578 방향?", "--option", "A|자문 먼저", "--option", "B|#79 먼저", "--option", "C|중단",
		"--recommended", "A", "--reason", "싸고 가역적", "--default-action", "보류하고 다음 태스크",
		"--default-trigger", "기한 경과 후 director 가 적용", "--due", "2026-09-25T18:00:00+09:00", "--doc", "task/2026-09-24/console-req-1",
		"--block", "--url", h.URL, "--token", "tok"}
	if err := run(args, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("out=%s", out.String())
	}
	if printed["recorded"] != true || printed["request_id"] != "dr-618-1" || !strings.Contains(printed["notify"].(string), "dr-618-1") {
		t.Fatalf("printed=%v", printed)
	}
	req := (*seen)[0]
	if req.Method != "POST" || req.Path != "/v1/tasks/618/decision-request" || req.Auth != "Bearer tok" {
		t.Fatalf("request=%+v", req)
	}
	body := req.Body
	options := body["options"].(map[string]any)["options"].([]any)
	if len(options) != 3 || options[0].(map[string]any)["recommended"] != true || body["default_action"] != "보류하고 다음 태스크" ||
		body["due_at"] != "2026-09-25T18:00:00+09:00" || body["block"] != true || body["doc"] != "task/2026-09-24/console-req-1" || body["reason"] != "싸고 가역적" {
		t.Fatalf("body=%v", body)
	}
	if _, present := body["requested_by"]; present {
		t.Fatalf("client must not claim the requester: %v", body)
	}
}

// Every refusal says NOT recorded and never that the request is visible;
// local validation refuses before any request leaves the process.
func TestTasksDecisionRequestCLIRefusals(t *testing.T) {
	h, seen := relaneServer(t, 409, `{"error":"decision_request_open"}`)
	base := []string{"tasks", "decision-request", "618", "--question", "q", "--default-action", "보류", "--url", h.URL, "--token", "tok"}
	cases := map[string]struct {
		extra []string
		want  string
	}{
		"zero options":   {nil, "at least one --option"},
		"41 hangul":      {[]string{"--option", "A|" + strings.Repeat("가", 41)}, "123 bytes; the limit is 120 bytes (not characters)"},
		"no default":     {[]string{"--option", "A|x", "--default-action", ""}, "default action is required"},
		"bad due":        {[]string{"--option", "A|x", "--due", "2026-09-25 18:00"}, "RFC3339"},
		"bad recommend":  {[]string{"--option", "A|x", "--recommended", "B"}, "--recommended must name"},
		"server refusal": {[]string{"--option", "A|x"}, "decision_request_open"},
	}
	for name, c := range cases {
		err := run(append(append([]string{}, base...), c.extra...), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "NOT recorded") || !strings.Contains(err.Error(), "do not notify") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("only the valid request may reach the server, got %d", len(*seen))
	}
	// Exactly 120 bytes of Hangul passes local validation.
	if err := run(append(append([]string{}, base...), "--option", "A|"+strings.Repeat("가", 40)), io.Discard, io.Discard); err == nil || strings.Contains(err.Error(), "bytes") {
		t.Fatalf("120-byte label: %v", err)
	}
	// Argument errors are refusals too: every one says NOT recorded.
	for name, args := range map[string][]string{
		"bad id":         {"tasks", "decision-request", "abc", "--question", "q"},
		"no id":          {"tasks", "decision-request", "--question", "q"},
		"unknown flag":   {"tasks", "decision-request", "618", "--nope"},
		"missing value":  {"tasks", "decision-request", "618", "--question"},
		"resolve bad id": {"tasks", "decision-resolve", "abc", "--request", "dr-1-1", "--kind", "withdrawn", "--text", "x"},
	} {
		err := run(args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "NOT recorded") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if err := run([]string{"tasks", "decision-request", "abc", "--question", "q"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "task id must be positive") || !strings.Contains(err.Error(), "do not notify") {
		t.Fatalf("bad id: %v", err)
	}
}

func TestTasksDecisionResolveCLI(t *testing.T) {
	h, seen := relaneServer(t, 200, `{"task":{"id":618,"state":"merged"},"request":{"id":"dr-618-1","status":"default_applied"},"duplicate":false}`)
	var out bytes.Buffer
	if err := run([]string{"tasks", "decision-resolve", "618", "--request", "dr-618-1", "--kind", "default_applied", "--receipt", "hk:doc receipt/x", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if req := (*seen)[0]; req.Path != "/v1/tasks/618/decision-request/resolve" || req.Body["receipt"] != "hk:doc receipt/x" || req.Body["kind"] != "default_applied" {
		t.Fatalf("request=%+v", req)
	}
	if err := run([]string{"tasks", "decision-resolve", "618", "--request", "dr-618-1", "--kind", "default_applied", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "requires a receipt") {
		t.Fatalf("no receipt: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("receipt-less application reached the server")
	}
}

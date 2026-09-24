package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// M1 (CodeRabbit, #48): a value flag never swallows the next flag. With
// "--reason --block" the old parser recorded reason="--block" and never set
// Block, so the request was written without blocking the task. Text that
// starts with "-" still passes in the "--reason=-text" form.
func TestTasksDecisionRequestCLIFlagIsNotAValue(t *testing.T) {
	h, seen := relaneServer(t, 201, decisionRequestOKBody)
	base := []string{"tasks", "decision-request", "618", "--question", "q", "--option", "A|x", "--default-action", "보류", "--url", h.URL, "--token", "tok"}
	err := run(append(append([]string{}, base...), "--reason", "--block"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--reason requires a value") || !strings.Contains(err.Error(), "NOT recorded") {
		t.Fatalf("flag taken as value: err=%v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("a request was sent: %+v", (*seen)[0].Body)
	}
	if err := run(append(append([]string{}, base...), "--reason=--not-a-flag", "--block"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if body := (*seen)[0].Body; body["reason"] != "--not-a-flag" || body["block"] != true {
		t.Fatalf("explicit = form: body=%v", body)
	}
}

// M2 (CodeRabbit, #48): once the POST has left the process, only a refusal
// the server actually sent proves nothing was written. A dropped connection
// after the server committed, an undecodable reply, a 5xx or the API's
// catch-all 400 are UNKNOWN (exit 4) — never "NOT recorded", never success.
func TestTasksDecisionWriteOutcomeUnknown(t *testing.T) {
	var committed atomic.Int32
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		committed.Add(1) // the server records the request, then the reply is lost
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(drop.Close)
	garbage, _ := relaneServer(t, 201, `{"task":`)
	gateway, _ := relaneServer(t, 502, `<html>bad gateway</html>`)
	catchAll, _ := relaneServer(t, 400, `{"error":"invalid_context"}`)
	refused, _ := relaneServer(t, 409, `{"error":"decision_request_open"}`)
	request := func(url string) []string {
		return []string{"tasks", "decision-request", "618", "--question", "q", "--option", "A|x", "--default-action", "보류", "--url", url, "--token", "tok"}
	}
	resolve := func(url string) []string {
		return []string{"tasks", "decision-resolve", "618", "--request", "dr-618-1", "--kind", "withdrawn", "--text", "x", "--url", url, "--token", "tok"}
	}
	for name, args := range map[string][]string{
		"request dropped after commit":    request(drop.URL),
		"resolution dropped after commit": resolve(drop.URL),
		"undecodable 201":                 request(garbage.URL),
		"502":                             request(gateway.URL),
		"400 invalid_context":             resolve(catchAll.URL),
	} {
		err := run(args, io.Discard, io.Discard)
		var exit exitCodeError
		if err == nil || !errors.As(err, &exit) || exit.code != exitWriteUnknown || !strings.Contains(err.Error(), "UNKNOWN") ||
			!strings.Contains(err.Error(), "tasks show 618") || strings.Contains(err.Error(), "NOT recorded") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if n := committed.Load(); n != 2 {
		t.Fatalf("drop server saw %d writes, want 2", n)
	}
	// A refusal the server sent is still NOT recorded (exit 1, not 4).
	err := run(request(refused.URL), io.Discard, io.Discard)
	var exit exitCodeError
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "NOT recorded") {
		t.Fatalf("409 refusal: err=%v", err)
	}
}

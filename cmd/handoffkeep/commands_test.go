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

func commandsCLIServer(t *testing.T, response string) (*httptest.Server, *[]decisionCLICall) {
	t.Helper()
	calls := []decisionCLICall{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decisionCLICall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		_ = json.NewDecoder(r.Body).Decode(&c.Body)
		calls = append(calls, c)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &calls
}

func TestCommandsListCLI(t *testing.T) {
	h, calls := commandsCLIServer(t, `{"commands":[{"id":7,"kind":"worker.message","state":"recorded"}]}`)
	var out bytes.Buffer
	if err := run([]string{"commands", "list", "--executor-lane", "lane-a", "--state", "acked", "--after-id", "3", "--limit", "5", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil || len(printed["commands"].([]any)) != 1 {
		t.Fatalf("out=%s err=%v", out.String(), err)
	}
	if c := (*calls)[0]; c.Method != "GET" || c.Path != "/v1/commands" || c.Query != "after_id=3&executor_lane=lane-a&limit=5&state=acked" {
		t.Fatalf("call=%+v", c)
	}
	for _, args := range [][]string{
		{"commands", "list"},
		{"commands", "list", "--executor-lane", "lane-a", "extra"},
	} {
		if err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "usage: commands") {
			t.Fatalf("%v err=%v", args, err)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("refused arguments reached the server: %+v", *calls)
	}
}

func TestCommandsReceiptCLI(t *testing.T) {
	h, calls := commandsCLIServer(t, `{"id":7,"state":"failed","last_error_code":"worker_rejected"}`)
	var out bytes.Buffer
	if err := run([]string{"commands", "receipt", "7", "--state", "failed", "--error-code", "worker_rejected", "--note", "거부함", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Method != "POST" || c.Path != "/v1/commands/7/receipt" || c.Body["state"] != "failed" || c.Body["error_code"] != "worker_rejected" || c.Body["note"] != "거부함" {
		t.Fatalf("call=%+v", c)
	}
	for _, args := range [][]string{
		{"commands", "receipt"},
		{"commands", "receipt", "0", "--state", "done"},
		{"commands", "receipt", "7"},
	} {
		if err := run(append(args, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Fatalf("%v: accepted", args)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("refused arguments reached the server: %+v", *calls)
	}
}

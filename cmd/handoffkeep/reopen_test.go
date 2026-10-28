package main

import (
	"bytes"
	"io"
	"testing"
)

func TestTasksReopenCLI(t *testing.T) {
	h, seen := relaneServer(t, 200, `{"id":42,"state":"reopened"}`)
	var out bytes.Buffer
	for _, reason := range []string{"", " \n"} {
		if err := run([]string{"tasks", "reopen", "42", "--reason", reason, "--url", h.URL, "--token", "tok"}, &out, io.Discard); err == nil {
			t.Fatal("missing reason accepted")
		}
	}
	if len(*seen) != 0 {
		t.Fatal("invalid reopen sent request")
	}
	if err := run([]string{"tasks", "reopen", "42", "--reason", "reverted", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Path != "/v1/tasks/42/reopen" || (*seen)[0].Body["reason"] != "reverted" {
		t.Fatalf("requests=%+v", *seen)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"state":"reopened"`)) {
		t.Fatalf("output=%s", &out)
	}
}

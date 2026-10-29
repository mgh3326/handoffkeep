package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTasksAddExpected(t *testing.T) {
	h, seen := projectServer(t, 201)
	var out bytes.Buffer
	err := run([]string{"tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment", "--expected", "1d", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Body["expected"] != "1d" {
		t.Fatalf("body=%+v", (*seen)[0].Body)
	}
	for _, args := range [][]string{{"tasks", "add", "--expected", "bad"}, {"tasks", "list", "--expected", "1d"}} {
		if err := run(args, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "--expected") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

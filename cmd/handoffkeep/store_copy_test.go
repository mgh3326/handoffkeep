package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestStoreCopyRejectsBlankTableSelection(t *testing.T) {
	var out bytes.Buffer
	err := storeCopyCmd([]string{
		"--from", "postgres://copy@localhost/handoffkeep",
		"--to", "mysql://copy@localhost/handoffkeep",
		"--tables", ",,",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "no tables selected") {
		t.Fatalf("storeCopyCmd error=%v, want no tables selected", err)
	}
	if out.Len() != 0 {
		t.Fatalf("storeCopyCmd wrote success output for an empty selection: %q", out.String())
	}
}

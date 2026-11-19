package main

import (
	"bytes"
	"strings"
	"testing"
)

// hk 5506: the requeue's argument rules fail before any database or YouTrack
// access, so they are checked here without either.
func TestYTOpsRequeueArgumentRules(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no --by", []string{"--op", "5"}, "usage: youtrack ops requeue"},
		{"blank --by", []string{"--by", "  ", "--op", "5"}, "usage: youtrack ops requeue"},
		{"zero op id", []string{"--by", "ops", "--op", "0"}, "op id must be positive"},
		{"non-numeric op id", []string{"--by", "ops", "--op", "x"}, "op id must be positive"},
		{"positional argument", []string{"--by", "ops", "5"}, "usage: youtrack ops requeue"},
		{"negative after-id", []string{"--by", "ops", "--after-id=-1"}, "usage: youtrack ops requeue"},
		{"limit over the bound", []string{"--by", "ops", "--limit", "100000"}, "usage: youtrack ops requeue"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := youtrackOpsRequeueCmd(c.args, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %q before refusing", out.String())
			}
		})
	}
}

// The ops command routes `requeue` to its own parser, and an op id list keeps
// the order it was given.
func TestYTOpsRequeueRouting(t *testing.T) {
	var out bytes.Buffer
	if err := youtrackOpsCmd([]string{"requeue", "--op", "0"}, &out); err == nil || !strings.Contains(err.Error(), "op id must be positive") {
		t.Fatalf("requeue not routed: %v", err)
	}
	var ids int64List
	for _, v := range []string{"9", "3"} {
		if err := ids.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 2 || ids[0] != 9 || ids[1] != 3 {
		t.Fatalf("ids %v", ids)
	}
}

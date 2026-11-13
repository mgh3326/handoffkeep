package main

import (
	"io"
	"slices"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestYTAuthorityKindsArg(t *testing.T) {
	all, err := ytAuthorityKindsArg("all")
	if err != nil || !slices.Equal(all, store.YTAuthorityKinds) {
		t.Fatalf("all = %v %v", all, err)
	}
	one, err := ytAuthorityKindsArg("comments")
	if err != nil || !slices.Equal(one, []store.YTAuthorityKind{store.YTAuthorityComments}) {
		t.Fatalf("comments = %v %v", one, err)
	}
	if _, err := ytAuthorityKindsArg("files"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, args := range [][]string{
		{"set", "comments", "youtrack", "--note", "n"},
		{"set", "comments", "youtrack", "--by", "operator"},
		{"set", "comments", "both", "--by", "operator", "--note", "n"},
		{"set", "files", "hk", "--by", "operator", "--note", "n"},
		{"show", "extra"},
		{"flip"},
	} {
		if err := youtrackAuthorityCmd(args, io.Discard); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

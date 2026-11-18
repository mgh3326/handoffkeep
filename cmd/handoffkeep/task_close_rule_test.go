package main

import (
	"testing"
)

func TestResolveCloseRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantID   int64
		wantRule string
		wantErr  bool
	}{
		{"explicit", []string{"4896", "explicit"}, 4896, "explicit", false},
		{"auto", []string{"4896", "auto"}, 4896, "auto", false},
		// The store owns the vocabulary, so an unknown rule reaches it; this
		// resolver only checks the shape of the arguments.
		{"unknown rule passes to the store", []string{"4896", "sometimes"}, 4896, "sometimes", false},
		{"bad id", []string{"0", "explicit"}, 0, "", true},
		{"not a number id", []string{"abc", "explicit"}, 0, "", true},
		{"too few args", []string{"4896"}, 0, "", true},
		{"too many args", []string{"4896", "explicit", "extra"}, 0, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, rule, err := resolveCloseRule(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
			if !tc.wantErr && (id != tc.wantID || rule != tc.wantRule) {
				t.Fatalf("got id=%d rule=%q, want id=%d rule=%q", id, rule, tc.wantID, tc.wantRule)
			}
		})
	}
}

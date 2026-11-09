package main

import (
	"testing"
	"time"
)

func TestSearchBackendFromEnvSelectsOnlyYouTrack(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"postgres":   "",
		"youtrack":   "youtrack",
		" youtrack ": "youtrack",
		"bogus":      "",
	}
	for in, want := range cases {
		t.Setenv("HANDOFFKEEP_SEARCH_BACKEND", in)
		if got := searchBackendFromEnv(); got != want {
			t.Fatalf("HANDOFFKEEP_SEARCH_BACKEND=%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSearchYouTrackTimeoutFromEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"":      0,
		"3s":    3 * time.Second,
		"250ms": 250 * time.Millisecond,
		"soon":  0,
		"-1s":   0,
	}
	for in, want := range cases {
		t.Setenv("HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT", in)
		if got := youtrackSearchTimeoutFromEnv(); got != want {
			t.Fatalf("HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT=%q -> %v, want %v", in, got, want)
		}
	}
}

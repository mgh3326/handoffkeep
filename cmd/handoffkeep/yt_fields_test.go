package main

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The fields mode alone starts the write worker (hk 4003), needs the
// YouTrack credentials like the other modes, and blocks the legacy push.
func TestYTFieldsModeStartsTheWriteWorker(t *testing.T) {
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_FIELDS_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("on without credentials: %v", err)
	}
	if owner, err := youtrackProtocolOwner(); err != nil || owner != "HANDOFFKEEP_YT_FIELDS_MODE=on" {
		t.Fatalf("protocol owner %q, %v", owner, err)
	}
	t.Setenv("YOUTRACK_URL", readthroughTestURL)
	t.Setenv("YOUTRACK_BOT_API_KEY", readthroughTestKey)
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil {
		t.Fatalf("fields on: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "On")
	if _, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_FIELDS_MODE") {
		t.Fatalf("an unknown fields mode: %v", err)
	}
	if _, err := youtrackProtocolOwner(); err == nil {
		t.Fatal("an unknown fields mode let the legacy push run")
	}
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w != nil {
		t.Fatalf("all off: worker %v err %v", w, err)
	}
	if owner, err := youtrackProtocolOwner(); err != nil || owner != "" {
		t.Fatalf("all off: owner %q, %v", owner, err)
	}
}

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestYTIssuesModeFromEnv(t *testing.T) {
	for value, want := range map[string]store.YTIssuesMode{
		"": store.YTIssuesOff, "off": store.YTIssuesOff, " off ": store.YTIssuesOff, "on": store.YTIssuesOn, " on\n": store.YTIssuesOn,
	} {
		t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", value)
		if got, err := ytIssuesModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"ON", "canary", "true", "1"} {
		t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", value)
		if got, err := ytIssuesModeFromEnv(); err == nil {
			t.Errorf("%q accepted as %q", value, got)
		}
	}
}

// The issues mode alone starts the write worker (the comments mode may stay
// off), and needs the YouTrack credentials like the comments mode.
func TestYTIssuesModeStartsTheWriteWorker(t *testing.T) {
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_ISSUES_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("on without credentials: %v", err)
	}
	t.Setenv("YOUTRACK_URL", readthroughTestURL)
	t.Setenv("YOUTRACK_BOT_API_KEY", readthroughTestKey)
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil {
		t.Fatalf("issues on: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", "bogus")
	if _, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil {
		t.Fatal("an unknown issues mode started")
	}
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w != nil {
		t.Fatalf("both off: worker %v err %v", w, err)
	}
}

func TestYouTrackIssuesBackfillArgs(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	for _, args := range [][]string{{"extra"}, {"--bogus"}} {
		if err := run(append([]string{"youtrack", "issues-backfill"}, args...), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "issues-backfill") {
			t.Errorf("%v: %v", args, err)
		}
	}
	err := run([]string{"yt", "issues-backfill", "--dry-run", "--project", "p"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_DB_URL") {
		t.Fatalf("no database: %v", err)
	}
}

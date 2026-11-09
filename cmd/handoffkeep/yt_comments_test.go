package main

import (
	"io"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestYTCommentsModeFromEnv(t *testing.T) {
	for value, want := range map[string]store.YTCommentsMode{"": store.YTCommentsOff, "off": store.YTCommentsOff, "canary": store.YTCommentsCanary, " canary ": store.YTCommentsCanary} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	// A value the binary does not know fails closed: an error, never a mode.
	for _, value := range []string{"on", "CANARY", "true", "all"} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err == nil {
			t.Errorf("%q accepted as %q", value, got)
		}
	}
}

// canary without YouTrack credentials is a startup error, never a silently
// idle writer; off needs no YouTrack configuration at all.
func TestYTCommentsWorkerNeedsCredentialsOnlyInCanary(t *testing.T) {
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(nil); err != nil || w != nil {
		t.Fatalf("off: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "canary")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("canary without credentials: %v", err)
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "bogus")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil {
		t.Fatal("an unknown mode started")
	}
}

// `handoffkeep yt …` is the youtrack command under the name the brief uses.
func TestYTIsTheYouTrackCommand(t *testing.T) {
	err := run([]string{"yt", "nonsense"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ops") || !strings.Contains(err.Error(), "canary") {
		t.Fatalf("yt usage: %v", err)
	}
	err = run([]string{"yt", "ops", "--state", "done"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("yt ops with a bad state: %v", err)
	}
}

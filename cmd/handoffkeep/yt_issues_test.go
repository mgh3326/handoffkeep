package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

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

// fakeFence records what one pass does with the issue fence.
type fakeFence struct {
	held               bool
	holds, sweeps, rel int
}

func (f *fakeFence) HoldYTIssueFence(context.Context) (func(), bool, error) {
	f.holds++
	if !f.held {
		return func() {}, false, nil
	}
	return func() { f.rel++ }, true, nil
}

func (f *fakeFence) RejectOwnedYTIssueOps(context.Context, time.Time) (int, error) {
	f.sweeps++
	return 0, nil
}

// A pass sweeps owned issue ops and releases the fence only when it took it.
func TestYTWriteWorkerHoldsTheIssueFence(t *testing.T) {
	held := &fakeFence{held: true}
	ytCommentsWorker{issues: held}.holdIssueFence(t.Context())()
	if held.holds != 1 || held.sweeps != 1 || held.rel != 1 {
		t.Fatalf("held: %+v", held)
	}
	busy := &fakeFence{}
	ytCommentsWorker{issues: busy}.holdIssueFence(t.Context())()
	if busy.holds != 1 || busy.sweeps != 0 || busy.rel != 0 {
		t.Fatalf("busy: %+v", busy)
	}
	ytCommentsWorker{}.holdIssueFence(t.Context())() // comments only: no fence
}

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestYTLinksModeFromEnv(t *testing.T) {
	for raw, want := range map[string]store.YTLinksMode{"": store.YTLinksOff, "off": store.YTLinksOff, " off ": store.YTLinksOff, "on": store.YTLinksOn, " on ": store.YTLinksOn} {
		t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", raw)
		if got, err := ytLinksModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"ON", "1", "true", "canary"} {
		t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", raw)
		if _, err := ytLinksModeFromEnv(); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// The links mode alone starts the write worker and needs the YouTrack
// credentials, like every other write mode; with it off nothing starts.
func TestYTLinksModeStartsWriteWorker(t *testing.T) {
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_YT_ISSUES_MODE", "HANDOFFKEEP_YT_FIELDS_MODE", "HANDOFFKEEP_YT_DOC_COMMENTS_MODE",
		"HANDOFFKEEP_YT_DECISIONS_MODE", "HANDOFFKEEP_YT_DOCS_MODE", "HANDOFFKEEP_YT_COMMENTS_CATCHUP"} {
		t.Setenv(name, "off")
	}
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w != nil {
		t.Fatalf("every mode off: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_LINKS_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("links on without credentials: %v", err)
	}
	t.Setenv("YOUTRACK_URL", serveTestURL)
	t.Setenv("YOUTRACK_BOT_API_KEY", serveTestKey)
	w, err := ytCommentsWorkerFromEnv(&store.Store{})
	if err != nil || w == nil {
		t.Fatalf("links on: worker %v err %v", w, err)
	}
	if cw, ok := w.(ytCommentsWorker); !ok || cw.links == nil {
		t.Fatalf("links on built a worker with no sweep: %#v", w)
	}
}

type recordingLinksSweeper struct {
	at    []time.Time
	limit []int
}

func (r *recordingLinksSweeper) RunYTLinksSweep(_ context.Context, now time.Time, limit int) (store.YTLinksSweepPass, error) {
	r.at, r.limit = append(r.at, now), append(r.limit, limit)
	return store.YTLinksSweepPass{}, nil
}

// The worker runs one sweep slice per pass, bounded by the sweep's per-pass
// size and stamped with the worker's clock; with no sweeper it runs none.
func TestYTWorkerRunsTheLinksSweep(t *testing.T) {
	clock := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)
	rec := &recordingLinksSweeper{}
	w := ytCommentsWorker{links: rec, now: func() time.Time { return clock }}
	w.sweepLinks(context.Background())
	if len(rec.at) != 1 || !rec.at[0].Equal(clock) || rec.limit[0] != store.YTLinksSweepPerPass {
		t.Fatalf("sweep calls %v %v", rec.at, rec.limit)
	}
	ytCommentsWorker{}.sweepLinks(context.Background())
}

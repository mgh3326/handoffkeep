package main

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestYTLinksModeFromEnv(t *testing.T) {
	for raw, want := range map[string]store.YTLinksMode{"": store.YTLinksOff, "off": store.YTLinksOff, " off ": store.YTLinksOff, "on": store.YTLinksOn, " on ": store.YTLinksOn} {
		t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", raw)
		if got, err := ytLinksModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"ON", "1", "true"} {
		t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", raw)
		if _, err := ytLinksModeFromEnv(); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestYTLinksModeStartsWriteWorker(t *testing.T) {
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_YT_ISSUES_MODE", "HANDOFFKEEP_YT_FIELDS_MODE", "HANDOFFKEEP_YT_LINKS_MODE", "HANDOFFKEEP_YT_DOC_COMMENTS_MODE", "HANDOFFKEEP_YT_DECISIONS_MODE", "HANDOFFKEEP_YT_DOCS_MODE", "HANDOFFKEEP_YT_COMMENTS_CATCHUP"} {
		t.Setenv(name, "off")
	}
	t.Setenv("HANDOFFKEEP_YT_LINKS_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_LINKS_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("links on without credentials: %v", err)
	}
	t.Setenv("YOUTRACK_URL", serveTestURL)
	t.Setenv("YOUTRACK_BOT_API_KEY", serveTestKey)
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil {
		t.Fatalf("links on: worker %v err %v", w, err)
	}
}

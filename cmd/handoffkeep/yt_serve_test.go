package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Fake credentials: nothing here reaches a network, and the tests assert that
// neither value appears in a log line.
const (
	serveTestURL = "https://serve-tenant.example.test"
	serveTestKey = "fake-serve-bot-key"
)

// serveEnv clears every variable serve's YouTrack wiring reads, then sets the
// credentials and the search selector.
func serveEnv(t *testing.T, url, key, search string) {
	t.Helper()
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_SEARCH_BACKEND", "HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT"} {
		t.Setenv(name, "")
	}
	t.Setenv("YOUTRACK_URL", url)
	t.Setenv("YOUTRACK_BOT_API_KEY", key)
	t.Setenv("HANDOFFKEEP_SEARCH_BACKEND", search)
}

// captureStdLog sends the standard logger to a buffer for the rest of t.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// The YouTrack search half needs HANDOFFKEEP_SEARCH_BACKEND=youtrack and both
// credentials; without them the selector is ignored with one log line.
func TestSearchBackendYouTrackNeedsCredentials(t *testing.T) {
	const ignored = "search.backend youtrack ignored reason=youtrack_credentials_unset"
	serveEnv(t, serveTestURL, serveTestKey, "youtrack")
	buf := captureStdLog(t)
	yt, err := serveYouTrackFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if yt.search == nil || yt.searchBackend != "youtrack" {
		t.Errorf("configured: search service %v backend %q, want a service and youtrack", yt.search != nil, yt.searchBackend)
	}
	if strings.Contains(buf.String(), "search.backend") {
		t.Errorf("configured search logged %q", buf.String())
	}

	serveEnv(t, "", "", "youtrack")
	buf = captureStdLog(t)
	yt, err = serveYouTrackFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if yt.search != nil || yt.searchBackend != "" {
		t.Errorf("no credentials: search service %v backend %q, want nil and empty", yt.search != nil, yt.searchBackend)
	}
	if n := strings.Count(buf.String(), ignored); n != 1 {
		t.Errorf("%d %q lines, want 1: %q", n, ignored, buf.String())
	}
	if strings.Contains(buf.String(), serveTestKey) {
		t.Errorf("log carries the key: %q", buf.String())
	}
}

// The postgres selector builds no YouTrack service and logs nothing.
func TestSearchBackendPostgresBuildsNoService(t *testing.T) {
	serveEnv(t, serveTestURL, serveTestKey, "")
	buf := captureStdLog(t)
	yt, err := serveYouTrackFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if yt.search != nil || yt.searchBackend != "" {
		t.Errorf("postgres selector: search service %v backend %q", yt.search != nil, yt.searchBackend)
	}
	if strings.Contains(buf.String(), "search.backend") {
		t.Errorf("postgres selector logged %q", buf.String())
	}
}

// The b1a comment worker reads its own credentials: serve starts it without a
// YouTrack search service.
// The zero store is enough: building the worker touches no database.
func TestYTCommentsCanaryStartsWithoutYouTrackSearch(t *testing.T) {
	serveEnv(t, serveTestURL, serveTestKey, "")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "canary")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil {
		t.Fatalf("comment worker: %v err %v, want a worker", w, err)
	}
	yt, err := serveYouTrackFromEnv(&store.Store{})
	if err != nil {
		t.Fatal(err)
	}
	if yt.search != nil {
		t.Error("serve built a search service without the youtrack selector")
	}
	if yt.comments == nil {
		t.Error("serve dropped the comment worker")
	}
}

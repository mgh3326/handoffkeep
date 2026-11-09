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
// neither value appears in an error or a log line.
const (
	readthroughTestURL = "https://readthrough-tenant.example.test"
	readthroughTestKey = "fake-readthrough-bot-key"
)

const readthroughOffLine = "youtrack.readthrough disabled reason=mode_off"

// readthroughEnv clears every variable serve's YouTrack wiring reads, then
// sets the credentials and the read-through mode.
func readthroughEnv(t *testing.T, url, key, mode string) {
	t.Helper()
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_SEARCH_BACKEND", "HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT", "YOUTRACK_DEFAULT_HK_PROJECT", "YOUTRACK_DEFAULT_LANE", "YOUTRACK_CONSOLE_URL"} {
		t.Setenv(name, "")
	}
	t.Setenv("YOUTRACK_URL", url)
	t.Setenv("YOUTRACK_BOT_API_KEY", key)
	t.Setenv("HANDOFFKEEP_YT_READTHROUGH_MODE", mode)
}

// captureStdLog sends the standard logger to a buffer for the rest of t.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func assertNoCredentials(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range []string{readthroughTestURL, "readthrough-tenant", readthroughTestKey} {
		if strings.Contains(text, secret) {
			t.Errorf("%s carries %q: %q", what, secret, text)
		}
	}
}

func TestYouTrackFromEnvReadthroughModes(t *testing.T) {
	// The credentials alone no longer turn the service on.
	for _, mode := range []string{"", "off", " off "} {
		readthroughEnv(t, readthroughTestURL, readthroughTestKey, mode)
		if svc, err := youTrackFromEnv(nil); err != nil || svc != nil {
			t.Errorf("mode %q with both keys: service %v err %v, want nil, nil", mode, svc != nil, err)
		}
	}
	// on, trimmed like HANDOFFKEEP_YT_COMMENTS_MODE, keeps today's service.
	for _, mode := range []string{"on", " on "} {
		readthroughEnv(t, readthroughTestURL, readthroughTestKey, mode)
		if svc, err := youTrackFromEnv(nil); err != nil || svc == nil {
			t.Errorf("mode %q with both keys: service %v err %v, want a service", mode, svc != nil, err)
		}
	}
	// Comments mode is case-sensitive, so this one is too: a value the binary
	// does not know fails startup, with or without the keys.
	for _, keys := range [][2]string{{readthroughTestURL, readthroughTestKey}, {"", ""}} {
		for _, mode := range []string{"ON", "On", "true", "1", "enabled", "canary", "of f"} {
			readthroughEnv(t, keys[0], keys[1], mode)
			svc, err := youTrackFromEnv(nil)
			if err == nil {
				t.Errorf("mode %q (keys set %t) accepted: service %v", mode, keys[0] != "", svc != nil)
				continue
			}
			if svc != nil {
				t.Errorf("mode %q: a service came back with the error", mode)
			}
			msg := err.Error()
			if !strings.Contains(msg, "HANDOFFKEEP_YT_READTHROUGH_MODE") || !strings.Contains(msg, "off or on") {
				t.Errorf("mode %q: error %q does not name the variable and its values", mode, msg)
			}
			assertNoCredentials(t, "mode error", msg)
		}
	}
}

func TestYouTrackFromEnvReadthroughOnRequiresBothKeys(t *testing.T) {
	cases := map[string][2]string{
		"url only":        {readthroughTestURL, ""},
		"key only":        {"", readthroughTestKey},
		"neither":         {"", ""},
		"blank url":       {"   ", readthroughTestKey},
		"blank key":       {readthroughTestURL, " \t "},
		"both whitespace": {" ", " "},
	}
	for name, keys := range cases {
		readthroughEnv(t, keys[0], keys[1], "on")
		svc, err := youTrackFromEnv(nil)
		if err == nil {
			t.Errorf("%s: on started (service %v), want a startup error", name, svc != nil)
			continue
		}
		if svc != nil {
			t.Errorf("%s: a service came back with the error", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "YOUTRACK_URL") || !strings.Contains(msg, "YOUTRACK_BOT_API_KEY") {
			t.Errorf("%s: error %q does not name the missing variables", name, msg)
		}
		assertNoCredentials(t, name+" error", msg)
	}
}

// Off never builds a client, so a URL the client would refuse cannot stop
// serve while read-through is off.
func TestYouTrackFromEnvReadthroughOffSkipsClientValidation(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		for _, url := range []string{"not a url", "http://tenant.example.test"} {
			readthroughEnv(t, url, readthroughTestKey, mode)
			if svc, err := youTrackFromEnv(nil); err != nil || svc != nil {
				t.Errorf("mode %q url %q: service %v err %v, want nil, nil", mode, url, svc != nil, err)
			}
		}
	}
	// The same URL with on is still refused, so the case above is the gate's.
	readthroughEnv(t, "not a url", readthroughTestKey, "on")
	if _, err := youTrackFromEnv(nil); err == nil {
		t.Fatal("on accepted a malformed YOUTRACK_URL")
	}
}

func TestYouTrackFromEnvReadthroughOffLogsOnce(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		readthroughEnv(t, readthroughTestURL, readthroughTestKey, mode)
		buf := captureStdLog(t)
		if svc, err := youTrackFromEnv(nil); err != nil || svc != nil {
			t.Fatalf("mode %q: service %v err %v", mode, svc != nil, err)
		}
		out := buf.String()
		if n := strings.Count(out, readthroughOffLine); n != 1 {
			t.Errorf("mode %q: %d %q lines, want 1: %q", mode, n, readthroughOffLine, out)
		}
		assertNoCredentials(t, "log", out)
	}
	// Without credentials there is nothing to explain.
	readthroughEnv(t, "", "", "")
	buf := captureStdLog(t)
	if svc, err := youTrackFromEnv(nil); err != nil || svc != nil {
		t.Fatalf("no keys: service %v err %v", svc != nil, err)
	}
	if strings.Contains(buf.String(), "youtrack.readthrough") {
		t.Errorf("no keys logged %q", buf.String())
	}
}

func TestSearchBackendYouTrackIgnoredWhenReadthroughOff(t *testing.T) {
	const ignored = "search.backend youtrack ignored reason=readthrough_off"
	readthroughEnv(t, readthroughTestURL, readthroughTestKey, "")
	t.Setenv("HANDOFFKEEP_SEARCH_BACKEND", "youtrack")
	buf := captureStdLog(t)
	yt, err := serveYouTrackFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if yt.readThrough != nil || yt.searchBackend == "youtrack" {
		t.Errorf("read-through off: service %v search backend %q, want nil and not youtrack", yt.readThrough != nil, yt.searchBackend)
	}
	out := buf.String()
	if n := strings.Count(out, ignored); n != 1 {
		t.Errorf("%d %q lines, want 1: %q", n, ignored, out)
	}
	assertNoCredentials(t, "log", out)

	// on keeps the selector, and says nothing about ignoring it.
	t.Setenv("HANDOFFKEEP_YT_READTHROUGH_MODE", "on")
	buf = captureStdLog(t)
	yt, err = serveYouTrackFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if yt.readThrough == nil || yt.searchBackend != "youtrack" {
		t.Errorf("read-through on: service %v search backend %q", yt.readThrough != nil, yt.searchBackend)
	}
	if strings.Contains(buf.String(), "search.backend") {
		t.Errorf("on logged %q", buf.String())
	}

	// The postgres selector with read-through off is not worth a line.
	t.Setenv("HANDOFFKEEP_YT_READTHROUGH_MODE", "off")
	t.Setenv("HANDOFFKEEP_SEARCH_BACKEND", "")
	buf = captureStdLog(t)
	if _, err := serveYouTrackFromEnv(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "search.backend") {
		t.Errorf("postgres selector logged %q", buf.String())
	}
}

// The b1a canary reads the credentials itself: with read-through left off,
// serve builds no read-through service and still starts the comment worker.
// The zero store is enough: building the worker touches no database.
func TestYTCommentsCanaryIndependentOfReadthrough(t *testing.T) {
	readthroughEnv(t, readthroughTestURL, readthroughTestKey, "")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "canary")
	if svc, err := youTrackFromEnv(nil); err != nil || svc != nil {
		t.Fatalf("read-through factory: service %v err %v, want nil, nil", svc != nil, err)
	}
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil {
		t.Fatalf("comment worker: %v err %v, want a worker", w, err)
	}
	yt, err := serveYouTrackFromEnv(&store.Store{})
	if err != nil {
		t.Fatal(err)
	}
	if yt.readThrough != nil {
		t.Error("serve built the read-through service for the canary")
	}
	if yt.comments == nil {
		t.Error("serve dropped the comment worker with read-through off")
	}
}

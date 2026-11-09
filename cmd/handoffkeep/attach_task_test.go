package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// `attach put FILE --task N` files the upload against the task, and a video
// is sent with its own MIME type (operator 2026-11-09: review captures and
// videos go to the task they review).
func TestAttachPutTaskRefAndVideoMIME(t *testing.T) {
	var gotRef, gotMIME string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRef, gotMIME = r.Header.Get("X-HK-Ref"), r.Header.Get("Content-Type")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"attachment":{"sha256":"ab"},"created":true}`)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	p := filepath.Join(t.TempDir(), "drive.mp4")
	if err := os.WriteFile(p, []byte("\x00\x00\x00\x18ftypmp42"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"attach", "put", p, "--task", "3310"}, &out, &out); err != nil {
		t.Fatalf("attach put: %v (%s)", err, out.String())
	}
	if gotRef != "task:3310" || gotMIME != "video/mp4" {
		t.Fatalf("ref=%q mime=%q, want task:3310 video/mp4", gotRef, gotMIME)
	}
}

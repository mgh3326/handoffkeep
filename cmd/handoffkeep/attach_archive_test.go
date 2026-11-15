package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/attachmentarchive"
)

// `attachments archive` reads the candidates page, copies and records nothing
// in a dry run, and with --apply downloads the bytes, writes the file, and
// posts the location relative to the root (design 2026-11-16/hk-attachment-archive).
func TestAttachArchiveDryRunThenApply(t *testing.T) {
	body := []byte("archive me")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	var recordBody map[string]any
	var downloads, records int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/attachments/archive-candidates":
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Errorf("limit = %q, want 100", got)
			}
			_, _ = io.WriteString(w, `{"candidates":[{"sha256":"`+sha+`","size_bytes":10,"mime":"text/plain","original_name":"note.txt","task_id":77,"tasks":[{"id":77,"state":"completed","closed_at":"2026-11-01T00:00:00Z"}]}],"next":""}`)
		case r.URL.Path == "/v1/attachments/"+sha && r.Method == http.MethodGet:
			downloads++
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", "attachment; filename=note.txt")
			_, _ = w.Write(body)
		case r.URL.Path == "/v1/attachments/"+sha+"/archive" && r.Method == http.MethodPost:
			records++
			if e := json.NewDecoder(r.Body).Decode(&recordBody); e != nil {
				t.Errorf("record body: %v", e)
			}
			_, _ = io.WriteString(w, `{"archived_at":"2026-11-15T01:02:03Z","location":"`+sha+`","hot_deleted":false}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")

	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, attachmentarchive.MarkerFile), nil, 0o600); e != nil {
		t.Fatal(e)
	}
	rel := filepath.ToSlash(filepath.Join("fleet-artifacts/archive/tasks/77/attachments", sha+"-note.txt"))

	var out bytes.Buffer
	if e := run([]string{"attachments", "archive", "--root", dir}, &out, &out); e != nil {
		t.Fatalf("dry run: %v (%s)", e, out.String())
	}
	var dry attachmentarchive.Summary
	if e := json.Unmarshal(out.Bytes(), &dry); e != nil {
		t.Fatalf("dry run output %q: %v", out.String(), e)
	}
	if !dry.DryRun || dry.Candidates != 1 || dry.Copied != 0 || dry.Recorded != 0 || len(dry.Planned) != 1 || dry.Planned[0].Path != rel {
		t.Fatalf("dry run summary = %+v", dry)
	}
	if downloads != 0 || records != 0 {
		t.Fatalf("dry run downloaded %d and recorded %d", downloads, records)
	}
	if _, e := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(e) {
		t.Fatalf("dry run wrote %s: %v", rel, e)
	}

	out.Reset()
	if e := run([]string{"attach", "archive", "--root", dir, "--apply"}, &out, &out); e != nil {
		t.Fatalf("apply: %v (%s)", e, out.String())
	}
	var applied attachmentarchive.Summary
	if e := json.Unmarshal(out.Bytes(), &applied); e != nil {
		t.Fatalf("apply output %q: %v", out.String(), e)
	}
	if applied.DryRun || applied.Copied != 1 || applied.Recorded != 1 || len(applied.Failed) != 0 {
		t.Fatalf("apply summary = %+v", applied)
	}
	if downloads != 1 || records != 1 {
		t.Fatalf("apply downloaded %d and recorded %d, want 1 and 1", downloads, records)
	}
	got, e := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if e != nil || !bytes.Equal(got, body) {
		t.Fatalf("stored copy = %q, err = %v", got, e)
	}
	if recordBody["sha256"] != sha || recordBody["location"] != rel || recordBody["size_bytes"] != float64(len(body)) {
		t.Fatalf("record body = %v", recordBody)
	}
}

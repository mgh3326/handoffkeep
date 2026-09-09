package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func writeManifestTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeManifestTestJSON(t *testing.T, path string, entries []docImportManifestEntry) {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	writeManifestTestFile(t, path, string(raw))
}

func decodeManifestResult(t *testing.T, output *bytes.Buffer) docImportManifestResult {
	t.Helper()
	var result docImportManifestResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", output.String(), err)
	}
	return result
}

func TestDocImportManifestOutUsesROBKeysAndSourceSHA(t *testing.T) {
	dir := t.TempDir()
	writeManifestTestFile(t, filepath.Join(dir, "ROB-11 First.md"), "first body")
	writeManifestTestFile(t, filepath.Join(dir, "nested", "ROB-22-second.md"), "second body")
	writeManifestTestFile(t, filepath.Join(dir, "nested", "ordinary.md"), "not imported")
	destination := filepath.Join(t.TempDir(), "candidate.json")
	var output bytes.Buffer
	if err := docCmd([]string{"import", "--dir", dir, "--manifest-out", destination}, &output); err != nil {
		t.Fatal(err)
	}
	var summary struct {
		DryRun  bool                       `json:"dry_run"`
		Matched int                        `json:"matched"`
		Skipped []docImportManifestSkipped `json:"skipped"`
	}
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.DryRun || summary.Matched != 2 || len(summary.Skipped) != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	entries, err := readImportManifest(destination)
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"linear/ROB-11", "linear/ROB-22"}
	if len(entries) != len(wantKeys) {
		t.Fatalf("entries=%+v", entries)
	}
	for index, entry := range entries {
		if entry.Key != wantKeys[index] || entry.Kind != "note" || !filepath.IsAbs(entry.Path) {
			t.Fatalf("entry[%d]=%+v", index, entry)
		}
		body, err := os.ReadFile(entry.Path)
		if err != nil {
			t.Fatal(err)
		}
		if entry.SHA256 != sha256Hex(body) {
			t.Fatalf("entry[%d] sha=%q want=%q", index, entry.SHA256, sha256Hex(body))
		}
	}
}

type manifestDocumentFake struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	docs     map[string]store.Document
	gets     int
	puts     int
	rejectOn string
}

func newManifestDocumentFake(t *testing.T) *manifestDocumentFake {
	t.Helper()
	fake := &manifestDocumentFake{t: t, docs: map[string]store.Document{}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *manifestDocumentFake) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fixture-token" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		fake.gets++
		document, found := fake.docs[key]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(document)
	case http.MethodPut:
		fake.puts++
		var document store.Document
		if err := json.NewDecoder(r.Body).Decode(&document); err != nil {
			fake.t.Errorf("decode document: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if fake.rejectOn != "" && strings.Contains(document.Body, fake.rejectOn) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"secret_like_content","pattern":"synthetic-secret"}`)
			return
		}
		document.Key = key
		document.SHA256 = sha256Hex([]byte(document.Body))
		previous, found := fake.docs[key]
		changed := !found || previous.SHA256 != document.SHA256
		fake.docs[key] = document
		_ = json.NewEncoder(w).Encode(map[string]any{"document": document, "changed": changed})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (fake *manifestDocumentFake) counts() (gets, puts int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.gets, fake.puts
}

func (fake *manifestDocumentFake) setRejectOn(value string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.rejectOn = value
}

func runManifestImport(t *testing.T, fake *manifestDocumentFake, manifest string) docImportManifestResult {
	t.Helper()
	var output bytes.Buffer
	err := docCmd([]string{
		"import", "--manifest", manifest, "--apply",
		"--url", fake.server.URL, "--token", "fixture-token",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	return decodeManifestResult(t, &output)
}

func TestDocImportManifestSafetyAndIdempotency(t *testing.T) {
	fake := newManifestDocumentFake(t)
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "ROB-31 first.md")
	writeManifestTestFile(t, firstPath, "stable source")
	manifest := filepath.Join(dir, "create.json")
	writeManifestTestJSON(t, manifest, []docImportManifestEntry{{
		Key: "linear/ROB-31", Path: firstPath, SHA256: sha256Hex([]byte("stable source")), Kind: "note",
	}})

	created := runManifestImport(t, fake, manifest)
	if len(created.Items) != 1 || created.Items[0].Status != "created" {
		t.Fatalf("created=%+v", created)
	}
	unchanged := runManifestImport(t, fake, manifest)
	if len(unchanged.Items) != 1 || unchanged.Items[0].Status != "unchanged" {
		t.Fatalf("unchanged=%+v", unchanged)
	}
	_, puts := fake.counts()
	if puts != 1 {
		t.Fatalf("idempotent import puts=%d", puts)
	}

	conflictPath := filepath.Join(dir, "ROB-31 conflict.md")
	writeManifestTestFile(t, conflictPath, "different source")
	conflictManifest := filepath.Join(dir, "conflict.json")
	writeManifestTestJSON(t, conflictManifest, []docImportManifestEntry{{
		Key: "linear/ROB-31", Path: conflictPath, SHA256: sha256Hex([]byte("different source")), Kind: "note",
	}})
	conflict := runManifestImport(t, fake, conflictManifest)
	if len(conflict.Items) != 1 || conflict.Items[0].Status != "conflict" {
		t.Fatalf("conflict=%+v", conflict)
	}
	_, currentPuts := fake.counts()
	if currentPuts != puts || fake.docs["linear/ROB-31"].Body != "stable source" {
		t.Fatalf("conflict mutated existing document: puts=%d body=%q", currentPuts, fake.docs["linear/ROB-31"].Body)
	}

	mismatchPath := filepath.Join(dir, "ROB-32 mismatch.md")
	writeManifestTestFile(t, mismatchPath, "actual source")
	mismatchManifest := filepath.Join(dir, "mismatch.json")
	writeManifestTestJSON(t, mismatchManifest, []docImportManifestEntry{{
		Key: "linear/ROB-32", Path: mismatchPath, SHA256: strings.Repeat("0", 64), Kind: "note",
	}})
	getsBefore, putsBefore := fake.counts()
	mismatch := runManifestImport(t, fake, mismatchManifest)
	if len(mismatch.Items) != 1 || mismatch.Items[0].Status != "sha_mismatch" {
		t.Fatalf("mismatch=%+v", mismatch)
	}
	getsAfter, putsAfter := fake.counts()
	if getsAfter != getsBefore || putsAfter != putsBefore {
		t.Fatalf("sha mismatch made a service call: before=%d/%d after=%d/%d", getsBefore, putsBefore, getsAfter, putsAfter)
	}

	duplicatePath := filepath.Join(dir, "duplicate.json")
	writeManifestTestJSON(t, duplicatePath, []docImportManifestEntry{
		{Key: "linear/ROB-33", Path: firstPath, SHA256: sha256Hex([]byte("stable source")), Kind: "note"},
		{Key: "linear/ROB-33", Path: conflictPath, SHA256: sha256Hex([]byte("different source")), Kind: "note"},
	})
	var duplicateOutput bytes.Buffer
	err := docCmd([]string{
		"import", "--manifest", duplicatePath, "--apply",
		"--url", fake.server.URL, "--token", "fixture-token",
	}, &duplicateOutput)
	if err == nil || !strings.Contains(err.Error(), firstPath) || !strings.Contains(err.Error(), conflictPath) {
		t.Fatalf("duplicate error=%v", err)
	}
	getsAfterDuplicate, putsAfterDuplicate := fake.counts()
	if getsAfterDuplicate != getsAfter || putsAfterDuplicate != putsAfter {
		t.Fatal("duplicate manifest was not rejected before service calls")
	}

	rejectedPath := filepath.Join(dir, "ROB-34 rejected.md")
	writeManifestTestFile(t, rejectedPath, "synthetic reject marker")
	rejectedManifest := filepath.Join(dir, "rejected.json")
	writeManifestTestJSON(t, rejectedManifest, []docImportManifestEntry{{
		Key: "linear/ROB-34", Path: rejectedPath, SHA256: sha256Hex([]byte("synthetic reject marker")), Kind: "note",
	}})
	fake.setRejectOn("synthetic reject marker")
	rejected := runManifestImport(t, fake, rejectedManifest)
	if len(rejected.Items) != 1 || rejected.Items[0].Status != "rejected" || rejected.Items[0].Pattern == "" {
		t.Fatalf("rejected=%+v", rejected)
	}
}

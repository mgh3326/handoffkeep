package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestListenValidation(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8800", "8.8.8.8:8800", "[::]:8800"} {
		if validListen(a, false) == nil {
			t.Fatalf("unsafe bind accepted: %s", a)
		}
	}
	if err := validListen("127.0.0.1:8800", false); err != nil {
		t.Fatal(err)
	}
	if err := validListen("100.122.100.56:8800", true); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryPushPullPreservesBytes(t *testing.T) {
	var saved store.Memory
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/memory/hk-probe/probe-2":
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(saved)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/hk-probe":
			_ = json.NewEncoder(w).Encode(map[string]any{"memory": []store.Memory{{Agent: saved.Agent, Name: saved.Name, Description: saved.Description, Type: saved.Type}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/hk-probe/probe-2":
			_ = json.NewEncoder(w).Encode(saved)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	source, destination := t.TempDir(), t.TempDir()
	raw := "---\nname: probe-2\ndescription: retained description\nmetadata:\n  type: reference\n---\n\n프로브 메모리 2 본문\n"
	if err := os.WriteFile(filepath.Join(source, "probe-2.md"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := memoryCmd([]string{"push", "--agent", "hk-probe", "--dir", source, "--apply"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if saved.Content != raw {
		t.Fatalf("push content=%q want raw=%q", saved.Content, raw)
	}
	if err := memoryCmd([]string{"pull", "--agent", "hk-probe", "--dir", destination, "--apply"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "probe-2.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(raw)) {
		t.Fatalf("round trip differs\ngot:  %q\nwant: %q", got, raw)
	}
}

func TestCheckpointRefsAndSearchFlagsAfterQuery(t *testing.T) {
	var checkpoint store.Checkpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkpoints":
			if err := json.NewDecoder(r.Body).Decode(&checkpoint); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(checkpoint)
		case "/v1/search":
			if r.URL.Query().Get("q") != "실증" || r.URL.Query().Get("session") != "hk-probe" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []store.SearchResult{{
				Scope: "ctx", Session: "hk-probe", Refs: store.Refs{"prs": []string{"26"}},
			}}})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	if err := ctxCmd([]string{"checkpoint", "--session", "hk-probe", "--title", "test", "--body", "body", "--ref", "prs=26", "--ref", "prs=27", "--ref", "jobs=hk-v0"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := store.Refs{"prs": []string{"26", "27"}, "jobs": []string{"hk-v0"}}
	if !reflect.DeepEqual(checkpoint.Refs, want) {
		t.Fatalf("refs=%#v", checkpoint.Refs)
	}
	var out bytes.Buffer
	if err := ctxCmd([]string{"search", "실증", "--session", "hk-probe"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := ctxCmd([]string{"search", "--session", "hk-probe", "실증"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"refs":{"prs":["26"]}`)) {
		t.Fatalf("search did not print refs: %s", out.String())
	}
}

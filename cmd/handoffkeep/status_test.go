package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestStatusCmd(t *testing.T) {
	body := `{"generated_at":"2026-11-06T00:00:00Z","project":"alpha","since":"2026-11-05T00:00:00Z","truncated":false,"counts":{"landable":1,"blocked":2,"waiting":3,"verifying":4,"total":10},"changes":[],"changed_since":{"count":1,"items":[{"change_id":1,"pr":"https://github.com/a/b/pull/42","what":"reviewed"}]},"gates":{"decisions_open":2,"held":1,"findings":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects/missing/frontier" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"project_not_found"}`)
			return
		}
		if r.URL.Path != "/v1/projects/alpha/frontier" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	var out bytes.Buffer
	if err := run([]string{"status", "--project", "alpha"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "alpha: 1 landable · 2 blocked · 3 waiting · 4 verifying\nsince 2026-11-05 09:00 KST: 1 changed (#42 reviewed)\ngates: 2 decisions open · 1 held · 1 findings\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	if err := run([]string{"status", "--project", "alpha", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var got, expected any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %v want %v", got, expected)
	}
	if err := run([]string{"status", "--project", "missing"}, &out, &bytes.Buffer{}); err == nil || err.Error() != "project_not_found" {
		t.Fatalf("404: %v", err)
	}
	if err := run([]string{"status"}, &out, &bytes.Buffer{}); err == nil || err.Error() != statusUsage {
		t.Fatalf("usage: %v", err)
	}
}

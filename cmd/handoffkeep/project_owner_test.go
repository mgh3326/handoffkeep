package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ownerServer answers the project owner routes and records what arrived.
func ownerServer(t *testing.T, status int) (*httptest.Server, *[]relaneRequest) {
	t.Helper()
	var seen []relaneRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := relaneRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if r.URL.RawQuery != "" {
			x.Path += "?" + r.URL.RawQuery
		}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 300 {
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		switch {
		case r.URL.Path == "/v1/tasks/projects/owners":
			_, _ = w.Write([]byte(`{"projects":[{"name":"handoffkeep","design_lane":"handoffkeep-design","effective_owner":"handoffkeep-design"},{"name":"wrk","design_lane":"","effective_owner":"operator-desk"}]}`))
		case strings.HasSuffix(r.URL.Path, "/owner") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"name":"wrk","design_lane":"wrk-design","effective_owner":"wrk-design","events":[{"id":3,"project":"wrk","previous_lane":"","design_lane":"wrk-design","actor":"operator","note":"decision/2026-11-07/x","created_at":"2026-11-07T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/owner"):
			lane, _ := x.Body["design_lane"].(string)
			resp, _ := json.Marshal(map[string]any{"name": "wrk", "design_lane": lane, "effective_owner": lane, "updated_at": "2026-11-07T00:00:00Z"})
			_, _ = w.Write(resp)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

// tasks owners lists every project's owner from GET /v1/tasks/projects/owners.
func TestTasksOwnersLists(t *testing.T) {
	h, seen := ownerServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "owners", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "GET" || req.Path != "/v1/tasks/projects/owners" || req.Auth != "Bearer tok" {
		t.Fatalf("request=%+v", req)
	}
	if !strings.Contains(out.String(), `"effective_owner":"operator-desk"`) {
		t.Fatalf("out=%s", out.String())
	}
}

// tasks owner <project> reads one project and its history; the client never
// sends an actor.
func TestTasksOwnerShowsHistory(t *testing.T) {
	h, seen := ownerServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "owner", "wrk", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "GET" || req.Path != "/v1/tasks/projects/wrk/owner" {
		t.Fatalf("request=%+v", req)
	}
	if !strings.Contains(out.String(), `"design_lane":"wrk-design"`) || !strings.Contains(out.String(), `"events":[`) {
		t.Fatalf("out=%s", out.String())
	}
}

// tasks owner --set sends the lane and the note to the PUT route.
func TestTasksOwnerSet(t *testing.T) {
	h, seen := ownerServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "owner", "wrk", "--set", "wrk-design", "--note", "decision/2026-11-07/design-session-desk-authority", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "PUT" || req.Path != "/v1/tasks/projects/wrk/owner" {
		t.Fatalf("request=%+v", req)
	}
	if req.Body["design_lane"] != "wrk-design" || req.Body["note"] != "decision/2026-11-07/design-session-desk-authority" {
		t.Fatalf("body=%v", req.Body)
	}
	if _, present := req.Body["actor"]; present {
		t.Fatalf("client must not send an actor: %v", req.Body)
	}
	if !strings.Contains(out.String(), `"design_lane":"wrk-design"`) {
		t.Fatalf("out=%s", out.String())
	}
}

// tasks owner --clear sends an explicit empty lane, keeping the note.
func TestTasksOwnerClear(t *testing.T) {
	h, seen := ownerServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "owner", "wrk", "--clear", "--note", "session closed", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	lane, ok := req.Body["design_lane"].(string)
	if req.Method != "PUT" || req.Path != "/v1/tasks/projects/wrk/owner" || !ok || lane != "" || req.Body["note"] != "session closed" {
		t.Fatalf("request=%+v body=%v", req, req.Body)
	}
}

// A missing project names the project instead of a bare not_found.
func TestTasksOwnerMissingProject(t *testing.T) {
	h, _ := ownerServer(t, http.StatusNotFound)
	var out bytes.Buffer
	err := run([]string{"tasks", "owner", "ghost", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no such project: ghost") {
		t.Fatalf("missing project err=%v", err)
	}
}

// --set with an empty lane is refused locally, and only one of --set/--clear
// may appear.
func TestTasksOwnerRefusesAmbiguousFlags(t *testing.T) {
	h, seen := ownerServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "owner", "wrk", "--set", "", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "--clear") {
		t.Fatalf("empty --set err=%v", err)
	}
	if err := run([]string{"tasks", "owner", "wrk", "--set", "wrk-design", "--clear", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both flags err=%v", err)
	}
	if err := run([]string{"tasks", "owner", "wrk", "--note", "orphan", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err == nil {
		t.Fatal("a bare --note was accepted")
	}
	if len(*seen) != 0 {
		t.Fatalf("refused invocations reached the server: %+v", *seen)
	}
}

// A pre-owner server 404s the route; the CLI names that as unsupported rather
// than surfacing a bare 404.
func TestTasksOwnersOldServerRefusal(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(plain.Close)
	var out bytes.Buffer
	err := run([]string{"tasks", "owners", "--url", plain.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "project_owners_unsupported") {
		t.Fatalf("old-server owners err=%v", err)
	}
}

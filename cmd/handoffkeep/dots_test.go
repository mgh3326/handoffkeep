package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDotsRequestCommand(t *testing.T) {
	var seen []intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/tasks/projects/owners" {
			_, _ = io.WriteString(w, `{"projects":[{"name":"experiment","design_lane":"desk","effective_owner":"desk"}]}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"task":{"id":5,"lane":"dots"}}`)
	}))
	t.Cleanup(h.Close)
	out, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate", "--requester-lane", "desk", "--project", "experiment", "--idempotency-key", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id":5`) || len(seen) != 1 {
		t.Fatalf("out=%s requests=%v", out, seen)
	}
	req := seen[0]
	if req.Path != "/v1/dots/requests" || string(req.Body["title"]) != `"Research"` || string(req.Body["body"]) != `"Please investigate"` || string(req.Body["requester_lane"]) != `"desk"` || string(req.Body["project"]) != `"experiment"` || string(req.Body["idempotency_key"]) != `"request-1"` {
		t.Fatalf("request=%+v", req)
	}
	if _, err := runAgainst(h, "dots", "request", "--title", "Research"); err == nil || len(seen) != 1 {
		t.Fatalf("missing body: err=%v requests=%v", err, seen)
	}
}

func TestDotsRequestDefaultsRequesterLaneAndUniqueProject(t *testing.T) {
	t.Setenv("PANEWIRE_LANE", "desk")
	var request intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tasks/projects/owners" {
			_, _ = io.WriteString(w, `{"projects":[{"name":"experiment","design_lane":"desk","effective_owner":"desk"}]}`)
			return
		}
		request = intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&request.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"task":{"id":5,"lane":"dots"}}`)
	}))
	t.Cleanup(h.Close)
	if _, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate"); err != nil {
		t.Fatal(err)
	}
	if string(request.Body["requester_lane"]) != `"desk"` || string(request.Body["project"]) != `"experiment"` {
		t.Fatalf("defaulted request body = %s", request.Body)
	}

	request = intakeRequest{}
	t.Setenv("PANEWIRE_LANE", "")
	if _, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate", "--requester-lane", "other"); err != nil {
		t.Fatal(err)
	}
	if string(request.Body["requester_lane"]) != `"other"` || len(request.Body["project"]) != 0 {
		t.Fatalf("override without configured project = %s", request.Body)
	}
	if _, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate", "--project", "explicit"); err == nil || !strings.Contains(err.Error(), "requester-lane or PANEWIRE_LANE") {
		t.Fatalf("missing requester lane error = %v", err)
	}
}

func TestDotsAnswerCommand(t *testing.T) {
	var seen []intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/dots/answer" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"task_id":4304,"request_id":"dr-4304-1","comment_id":12,"task":{"id":4304,"awaiting_decision":false}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(h.Close)

	out, err := runAgainst(h, "dots", "answer", "4304", "--text", "use option B")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"dr-4304-1"`) || len(seen) != 1 {
		t.Fatalf("out=%s requests=%v", out, seen)
	}
	req := seen[0]
	if req.Path != "/v1/dots/answer" || string(req.Body["text"]) != `"use option B"` {
		t.Fatalf("unexpected request: %+v", req)
	}

	if _, err := runAgainst(h, "dots", "answer"); err == nil || !strings.Contains(err.Error(), "dots answer requires a task id") {
		t.Fatalf("expected missing task id error, got %v", err)
	}
	if _, err := runAgainst(h, "dots", "answer", "4304"); err == nil || !strings.Contains(err.Error(), "dots answer requires --text") {
		t.Fatalf("expected missing text error, got %v", err)
	}
}


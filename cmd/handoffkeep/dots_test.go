package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
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
	var req intakeRequest
	for _, x := range seen {
		if x.Path == "/v1/dots/answer" {
			req = x
		}
	}
	if !strings.Contains(out, `"dr-4304-1"`) || req.Path == "" {
		t.Fatalf("out=%s requests=%v", out, seen)
	}
	if string(req.Body["text"]) != `"use option B"` {
		t.Fatalf("unexpected request: %+v", req)
	}
	if _, ok := req.Body["requester_lane"]; ok {
		t.Fatalf("a legacy server was sent requester_lane: %+v", req.Body)
	}

	if _, err := runAgainst(h, "dots", "answer"); err == nil || !strings.Contains(err.Error(), "dots answer requires a task id") {
		t.Fatalf("expected missing task id error, got %v", err)
	}
	if _, err := runAgainst(h, "dots", "answer", "4304"); err == nil || !strings.Contains(err.Error(), "dots answer requires --text") {
		t.Fatalf("expected missing text error, got %v", err)
	}
}

// A server that lists the plain-question shape gets the lane and the target.
func TestDotsAnswerCommandSendsQuestionFields(t *testing.T) {
	t.Setenv("PANEWIRE_LANE", "requester")
	var body map[string]any
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": capabilities.Features})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"task_id":4304,"comment_id":12,"task":{"id":4304}}`)
	}))
	t.Cleanup(h.Close)

	if _, err := runAgainst(h, "dots", "answer", "4304", "--text", "the van", "--question-id", "77"); err != nil {
		t.Fatal(err)
	}
	if body["requester_lane"] != "requester" || body["question_id"] != float64(77) {
		t.Fatalf("answer body=%v", body)
	}
}

// Invariant (operator decision 346): a refused dots request prints the
// server's reason, not a bare invalid_context.
func TestDotsRequestRefusalPrintsReason(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/tasks/projects/owners" {
			_, _ = io.WriteString(w, `{"projects":[]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_context","reason":"invalid dots request type: CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION"}`)
	}))
	t.Cleanup(h.Close)
	_, err := runAgainst(h, "dots", "request", "--requester-lane", "x", "--type", "NO_SUCH_TYPE", "--title", "x", "--body", "y")
	want := "invalid_context: invalid dots request type: CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestDotsReadsCommand(t *testing.T) {
	var path string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"reads":[
			{"id":2,"read_at":"2026-11-18T10:10:00Z","principal":"operator:a@example.com","listed":3,"result":"ok"},
			{"id":1,"read_at":"2026-11-18T10:00:00Z","principal":"operator:a@example.com","listed":3,"result":"ok"}
		]}`)
	}))
	t.Cleanup(h.Close)

	out, err := runAgainst(h, "dots", "reads", "--since", "2026-11-18T09:00:00Z", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/v1/dots/inbox-reads?") || !strings.Contains(path, "since=2026-11-18T09%3A00%3A00Z") || !strings.Contains(path, "limit=5") {
		t.Fatalf("request path = %s", path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("out is not JSON: %v: %s", err, out)
	}
	if body["last_read_at"] != "2026-11-18T10:10:00Z" || body["gap_seconds"] != float64(600) {
		t.Fatalf("summary = %v", body)
	}
	if reads, _ := body["reads"].([]any); len(reads) != 2 {
		t.Fatalf("reads = %v", body["reads"])
	}

	if _, err := runAgainst(h, "dots", "reads", "--since", "yesterday"); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("bad since err = %v", err)
	}
	if _, err := runAgainst(h, "dots", "reads", "--limit", "-1"); err == nil || !strings.Contains(err.Error(), "--limit") {
		t.Fatalf("bad limit err = %v", err)
	}
	if _, err := runAgainst(h, "dots", "reads", "extra"); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("positional err = %v", err)
	}
}

func TestDotsCancelCommand(t *testing.T) {
	var seen []intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/dots/requests/4933/cancel" || string(x.Body["requester_lane"]) == `"other-lane"` {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"task_conflict","reason":"only the request's requester lane may cancel it"}`)
			return
		}
		_, _ = io.WriteString(w, `{"task":{"id":4933,"state":"dropped","category":"canceled"}}`)
	}))
	t.Cleanup(h.Close)

	out, err := runAgainst(h, "dots", "cancel", "4933", "--reason", "operator allowed real-model resemblance", "--requester-lane", "parkingarcade-design")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"state":"dropped"`) || len(seen) != 1 {
		t.Fatalf("out=%s requests=%v", out, seen)
	}
	if seen[0].Path != "/v1/dots/requests/4933/cancel" || string(seen[0].Body["requester_lane"]) != `"parkingarcade-design"` || string(seen[0].Body["reason"]) != `"operator allowed real-model resemblance"` {
		t.Fatalf("request=%+v", seen[0])
	}

	t.Setenv("PANEWIRE_LANE", "desk")
	if _, err := runAgainst(h, "dots", "cancel", "4933", "--reason", "not needed"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || string(seen[1].Body["requester_lane"]) != `"desk"` {
		t.Fatalf("defaulted requester lane: %+v", seen)
	}

	_, err = runAgainst(h, "dots", "cancel", "4933", "--reason", "not needed", "--requester-lane", "other-lane")
	want := "task_conflict: only the request's requester lane may cancel it"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal err = %v, want %q", err, want)
	}

	if _, err := runAgainst(h, "dots", "cancel", "4933"); err == nil || !strings.Contains(err.Error(), "dots cancel requires --reason") {
		t.Fatalf("missing reason error = %v", err)
	}
	t.Setenv("PANEWIRE_LANE", "")
	if _, err := runAgainst(h, "dots", "cancel", "4933", "--reason", "not needed"); err == nil || !strings.Contains(err.Error(), "requester-lane or PANEWIRE_LANE") {
		t.Fatalf("missing lane error = %v", err)
	}
	if _, err := runAgainst(h, "dots", "cancel"); err == nil || !strings.Contains(err.Error(), "dots cancel requires a task id") {
		t.Fatalf("missing task id error = %v", err)
	}
}

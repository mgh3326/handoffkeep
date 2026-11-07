package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestDotsRequestCommand(t *testing.T) {
	h, seen := intakeServer(t, http.StatusCreated, `{"task":{"id":5,"lane":"dots"}}`)
	out, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate", "--requester-lane", "desk", "--project", "experiment")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id":5`) || len(*seen) != 1 {
		t.Fatalf("out=%s requests=%v", out, *seen)
	}
	req := (*seen)[0]
	if req.Path != "/v1/dots/requests" || string(req.Body["title"]) != `"Research"` || string(req.Body["body"]) != `"Please investigate"` || string(req.Body["requester_lane"]) != `"desk"` || string(req.Body["project"]) != `"experiment"` {
		t.Fatalf("request=%+v", req)
	}
	if _, err := runAgainst(h, "dots", "request", "--title", "Research"); err == nil || len(*seen) != 1 {
		t.Fatalf("missing body: err=%v requests=%v", err, *seen)
	}
}

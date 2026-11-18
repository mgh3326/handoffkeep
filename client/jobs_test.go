package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/client"
)

// The job fence calls (hk 5276): a read carries the owner, and a refused
// conditional update is branched on by its code, never its text.
func TestFleetJobOwnerFence(t *testing.T) {
	hk := newFakeHK(t)
	hk.on("GET", "/v1/jobs/job-7", http.StatusOK, `{"job_id":"job-7","task_id":42,"host":"host-a","state":"spawned","owner_node":"node-a","owner_epoch":2}`, nil)
	hk.on("GET", "/v1/jobs/job-gone", http.StatusNotFound, `{"error":"not_found"}`, nil)
	hk.on("PUT", "/v1/jobs/job-7", http.StatusConflict, `{"error":"job_owner_mismatch","owner_node":"node-a","owner_epoch":2}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}
	ctx := context.Background()

	job, found, err := c.GetJob(ctx, "job-7")
	if err != nil || !found || job.OwnerNode != "node-a" || job.OwnerEpoch != 2 || job.TaskID == nil || *job.TaskID != 42 {
		t.Fatalf("GetJob: %+v found=%t err=%v", job, found, err)
	}
	if _, found, err = c.GetJob(ctx, "job-gone"); err != nil || found {
		t.Fatalf("missing job: found=%t err=%v", found, err)
	}

	state := "spawned"
	_, err = c.UpdateJob(ctx, "job-7", client.JobUpdate{
		State:         &state,
		ExpectedOwner: &client.JobOwner{Node: "node-b", Epoch: 2},
	})
	if client.ErrorCode(err) != "job_owner_mismatch" {
		t.Fatalf("UpdateJob code=%q err=%v", client.ErrorCode(err), err)
	}

	// The request sends the fence as the server reads it; absent fields are null.
	var sent map[string]any
	reqs := hk.requests()
	last := reqs[len(reqs)-1]
	if last.method != "PUT" || last.uri != "/v1/jobs/job-7" {
		t.Fatalf("request: %s %s", last.method, last.uri)
	}
	if err := json.Unmarshal(last.body, &sent); err != nil {
		t.Fatal(err)
	}
	expected, _ := sent["expected_owner"].(map[string]any)
	if sent["state"] != "spawned" || sent["owner"] != nil || expected["node"] != "node-b" || expected["epoch"] != float64(2) {
		t.Fatalf("sent body: %s", last.body)
	}
}
